// Package list contains list functions
package list

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
	"github.com/rclone/rclone/fs/filter"
	"github.com/rclone/rclone/lib/bucket"
)

// RemoteEscapesRoot reports whether remote, taken as a path relative to an Fs
// root, climbs above that root when joined onto it.
//
// It mirrors what the backends actually do - path.Join(root, remote) - by
// joining remote onto a sentinel root and checking whether the result is still
// under the sentinel.
//
// A well behaved backend never produces such a name; one can arise from a
// malicious or buggy server (an object store permits keys containing ".."),
// and acting on it would let a listing or transfer escape the configured root.
func RemoteEscapesRoot(remote string) bool {
	const sentinel = "\x00rootsentinel"
	joined := path.Join(sentinel, remote)
	return joined != sentinel && !strings.HasPrefix(joined, sentinel+"/")
}

// RemoveEscaping drops - and logs - any entries whose Remote escapes the Fs
// root (see RemoteEscapesRoot), filtering the slice in place. It is applied to
// every listing unconditionally, independent of the include/exclude filters, so
// that names which cannot be safely confined are never surfaced to any
// operation.
func RemoveEscaping(entries fs.DirEntries) fs.DirEntries {
	kept := entries[:0]
	for _, entry := range entries {
		if RemoteEscapesRoot(entry.Remote()) {
			fs.Errorf(entry, "Entry %q escapes the root - ignoring", entry.Remote())
			continue
		}
		kept = append(kept, entry)
	}
	return kept
}

// DirSorted reads Object and *Dir into entries for the given Fs.
//
// dir is the start directory, "" for root
//
// If includeAll is specified all files will be added, otherwise only
// files and directories passing the filter will be added.
//
// Files will be returned in sorted order
func DirSorted(ctx context.Context, f fs.Fs, includeAll bool, dir string) (entries fs.DirEntries, err error) {
	// Get unfiltered entries from the fs
	entries, err = f.List(ctx, dir)
	accounting.Stats(ctx).Listed(int64(len(entries)))
	if err != nil {
		return nil, err
	}
	// This should happen only if exclude files lives in the
	// starting directory, otherwise ListDirSorted should not be
	// called.
	fi := filter.GetConfig(ctx)
	if !includeAll && fi.ListContainsExcludeFile(entries) {
		fs.Debugf(dir, "Excluded")
		return nil, nil
	}
	return filterAndSortDir(ctx, entries, includeAll, dir, fi.IncludeObject, fi.IncludeDirectory(ctx, f), fi)
}

// listP for every backend
func listP(ctx context.Context, f fs.Fs, dir string, callback fs.ListRCallback) error {
	if doListP := f.Features().ListP; doListP != nil {
		return doListP(ctx, dir, callback)
	}
	// Fallback to List
	entries, err := f.List(ctx, dir)
	if err != nil {
		return err
	}
	return callback(entries)
}

// DirSortedFn reads Object and *Dir into entries for the given Fs.
//
// dir is the start directory, "" for root
//
// If includeAll is specified all files will be added, otherwise only
// files and directories passing the filter will be added.
//
// Files will be returned through callback in sorted order
func DirSortedFn(ctx context.Context, f fs.Fs, includeAll bool, dir string, callback fs.ListRCallback, keyFn KeyFn) (err error) {
	stats := accounting.Stats(ctx)
	fi := filter.GetConfig(ctx)

	// Sort the entries, in or out of memory
	sorter, err := NewSorter(ctx, f, callback, keyFn)
	if err != nil {
		return fmt.Errorf("failed to create directory sorter: %w", err)
	}
	defer sorter.CleanUp()

	// Get unfiltered entries from the fs
	err = listP(ctx, f, dir, func(entries fs.DirEntries) error {
		stats.Listed(int64(len(entries)))

		// This should happen only if exclude files lives in the
		// starting directory, otherwise ListDirSorted should not be
		// called.
		if !includeAll && fi.ListContainsExcludeFile(entries) {
			fs.Debugf(dir, "Excluded")
			return nil
		}

		entries, err := filterDir(ctx, entries, includeAll, dir, fi.IncludeObject, fi.IncludeDirectory(ctx, f), fi)
		if err != nil {
			return err
		}
		return sorter.Add(entries)
	})
	if err != nil {
		return err
	}
	return sorter.Send()
}

// Filter the entries passed in
func filterDir(ctx context.Context, entries fs.DirEntries, includeAll bool, dir string,
	IncludeObject func(ctx context.Context, o fs.Object) bool,
	IncludeDirectory func(remote string) (bool, error),
	fi *filter.Filter) (newEntries fs.DirEntries, err error) {
	entries = RemoveEscaping(entries)
	newEntries = entries[:0] // in place filter
	// Uncomment to bypass batch evaluation when backend pre-filtered.
	// WARNING: This skips ALL filterDir processing, including:
	// - Post-hoc modtime/size/metadata guards (--min-age, --max-age, --min-size, --max-size, --metadata-filter)
	// - Directory filtering via IncludeDirectory
	// - Entry-shape validation
	// - Exclude-file checks (ListContainsExcludeFile)
	// Only safe when none of these features are in use.
	// if filter.GetPreFiltered(ctx) { return entries, nil }
	prefix := ""
	if dir != "" {
		prefix = dir
		if !bucket.IsAllSlashes(dir) {
			prefix += "/"
		}
	}

	// When batch evaluation is available and filtering is active,
	// collect validated objects and their indices for batch processing.
	useBatch := fi != nil && !includeAll && !fi.InActive()

	// Phase 1: validate all entries and filter directories.
	// Objects are collected for batch evaluation if useBatch is true.
	// Use a bool slice indexed by entry position to preserve original order.
	inc := make([]bool, len(entries))
	type objectEntry struct {
		obj      fs.Object
		entryIdx int // position in entries slice
	}
	var batchObjects []objectEntry

	for i, entry := range entries {
		// Check for known types first - unknown types are always an error
		switch entry.(type) {
		case fs.Object, fs.Directory:
			// ok - known type
		default:
			return nil, fmt.Errorf("unknown object type %T", entry)
		}

		// check remote name belongs in this directory - BEFORE any filter evaluation
		remote := entry.Remote()
		switch {
		case !strings.HasPrefix(remote, prefix):
			fs.Errorf(entry, "Entry doesn't belong in directory %q (too short) - ignoring", dir)
			continue
		case remote == dir:
			fs.Errorf(entry, "Entry doesn't belong in directory %q (same as directory) - ignoring", dir)
			continue
		case strings.ContainsRune(remote[len(prefix):], '/') && !bucket.IsAllSlashes(remote[len(prefix):]):
			fs.Errorf(entry, "Entry doesn't belong in directory %q (contains subdir) - ignoring", dir)
			continue
		}

		// check includes and types
		switch x := entry.(type) {
		case fs.Object:
			if useBatch {
				// Defer object filtering to batch evaluation
				batchObjects = append(batchObjects, objectEntry{obj: x, entryIdx: i})
			} else {
				// Per-object evaluation (includeAll or no batch)
				if !includeAll && !IncludeObject(ctx, x) {
					fs.Debugf(x, "Excluded")
					continue
				}
				inc[i] = true
			}
		case fs.Directory:
			if !includeAll {
				incDir, err := IncludeDirectory(x.Remote())
				if err != nil {
					return nil, err
				}
				if !incDir {
					fs.Debugf(x, "Excluded")
					continue
				}
			}
			inc[i] = true
		}
	}

	// Phase 2: batch-evaluate collected objects and mark results by original index.
	if useBatch && len(batchObjects) > 0 {
		objects := make([]fs.Object, len(batchObjects))
		for j, oe := range batchObjects {
			objects[j] = oe.obj
		}
		// IncludeObjectBatch handles its own "Excluded" debug logging
		results := fi.IncludeObjectBatch(ctx, prefix, objects)
		for j, oe := range batchObjects {
			inc[oe.entryIdx] = results[j]
		}
	}

	// Phase 3: single-pass rebuild preserving original interleaved order.
	for i, entry := range entries {
		if inc[i] {
			newEntries = append(newEntries, entry)
		}
	}

	return newEntries, nil
}

// filter and sort the entries
func filterAndSortDir(ctx context.Context, entries fs.DirEntries, includeAll bool, dir string,
	IncludeObject func(ctx context.Context, o fs.Object) bool,
	IncludeDirectory func(remote string) (bool, error),
	fi *filter.Filter) (newEntries fs.DirEntries, err error) {
	// Filter the directory entries (in place)
	entries, err = filterDir(ctx, entries, includeAll, dir, IncludeObject, IncludeDirectory, fi)
	if err != nil {
		return nil, err
	}

	// Sort the directory entries by Remote
	//
	// We use a stable sort here just in case there are
	// duplicates. Assuming the remote delivers the entries in a
	// consistent order, this will give the best user experience
	// in syncing as it will use the first entry for the sync
	// comparison.
	sort.Stable(entries)
	return entries, nil
}
