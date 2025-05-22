package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
	"github.com/rclone/rclone/fs/filter"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/lib/join"
)

const (
	readDirTimeout = 120
)

func (f *Fs) listFileInfos(ctx context.Context, fd *os.File, statFunc func(entry os.DirEntry) os.FileInfo) (fis []os.FileInfo, err error) {
	defer func() {
		if r := recover(); r != nil {
			fis = nil // Reset file infos to nil
			var panicErr error
			// If we panic, we want to return an error if available
			if e, ok := r.(error); ok {
				panicErr = fmt.Errorf("panic reading directory: %w", e)
			} else {
				panicErr = fmt.Errorf("panic reading directory: %v", r)
			}
			if err == nil {
				err = panicErr
			} else {
				fs.Errorf(f.Name(), "%v", panicErr)
			}
		}
	}()

	// Create a child context with timeout
	ctx, cancel := context.WithTimeout(ctx, readDirTimeout*time.Second)
	defer cancel()

	// Set read deadline if supported
	deadline, hasDeadline := ctx.Deadline()
	if hasDeadline {
		err = fd.SetReadDeadline(deadline)
		if err == nil || !errors.Is(err, os.ErrNoDeadline) {
			defer func() {
				_ = fd.SetReadDeadline(time.Time{})
			}()
		}
	}

	if useReadDir {
		type result struct {
			fis []os.FileInfo
			err error
		}
		resultCh := make(chan result, 1)

		go func() {
			defer func() {
				if r := recover(); r != nil {
					var panicErr error
					// If we panic, we want to return an error if available
					if e, ok := r.(error); ok {
						panicErr = fmt.Errorf("panic reading directory: %w", e)
					} else {
						panicErr = fmt.Errorf("panic reading directory: %v", r)
					}
					select {
					case resultCh <- result{nil, panicErr}:
					case <-ctx.Done():
					}
				}
			}()

			// Check context before starting
			select {
			case <-ctx.Done():
				return
			default:
			}

			// Windows and Plan9 read the directory entries with the stat information in which
			// shouldn't fail because of unreadable entries.
			fis, err := fd.Readdir(1024)
			select {
			case resultCh <- result{fis, err}:
			case <-ctx.Done():
				// Context canceled, don't send result
			}
		}()

		select {
		case res := <-resultCh:
			return res.fis, res.err
		case <-ctx.Done():
			return nil, fmt.Errorf("timeout reading directory: %w", ctx.Err())
		}

	} else {
		var entries []os.DirEntry

		type result struct {
			entries []os.DirEntry
			err     error
		}
		resultCh := make(chan result, 1)

		go func() {
			defer func() {
				if r := recover(); r != nil {
					var panicErr error
					// If we panic, we want to return an error if available
					if e, ok := r.(error); ok {
						panicErr = fmt.Errorf("panic reading directory: %w", e)
					} else {
						panicErr = fmt.Errorf("panic reading directory: %v", r)
					}
					select {
					case resultCh <- result{nil, panicErr}:
					case <-ctx.Done():
					}
				}
			}()

			// Check context before starting
			select {
			case <-ctx.Done():
				return
			default:
			}

			// For other OSes we read the names only (which shouldn't fail) then stat the
			// individual ourselves so we can log errors but not fail the directory read.
			entries, err := fd.ReadDir(1024)
			select {
			case resultCh <- result{entries, err}:
			case <-ctx.Done():
				// Context canceled, don't send result
			}
		}()

		select {
		case res := <-resultCh:
			entries, err = res.entries, res.err
		case <-ctx.Done():
			return nil, fmt.Errorf("timeout reading directory: %w", ctx.Err())
		}

		if err == nil {
			// TODO: Parallelise this loop
			for _, entry := range entries {
				fi := statFunc(entry)
				if fi != nil {
					fis = append(fis, fi)
				}
			}
		}
	}

	return fis, err
}

// List the objects and directories in dir into entries. The
// entries can be returned in any order but should be for a
// complete directory.
//
// dir should be "" to list the root, and should not have
// trailing slashes.
//
// This should return ErrDirNotFound if the directory isn't
// found.
func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	filter, useFilter := filter.GetConfig(ctx), filter.GetUseFilter(ctx)

	fsDirPath := f.localPath(dir)
	_, err = os.Stat(fsDirPath)
	if err != nil {
		return nil, fs.ErrorDirNotFound
	}

	var fd *os.File
	openDir := func() (*os.File, error) {
		return os.Open(fsDirPath)
	}
	closeDir := func() {
		if fd != nil {
			cerr := fd.Close()
			if cerr != nil {
				if err == nil {
					err = fmt.Errorf("failed to close directory %q: %w", dir, cerr)
				} else {
					fs.Errorf(dir, "Failed to close directory: %v", cerr)
				}
			}
			fd = nil
		}
	}
	defer closeDir()

	for {
		if fd == nil {
			fd, err = openDir()
			if err != nil {
				isPerm := os.IsPermission(err)
				err = fmt.Errorf("failed to open directory %q: %w", dir, err)
				fs.Errorf(dir, "%v", err)
				if isPerm {
					_ = accounting.Stats(ctx).Error(fserrors.NoRetryError(err))
					err = nil // Ignore error but fail sync
				}
				return nil, err
			}
			entries = nil // Reset entries to start from beginning
		}

		var fis []os.FileInfo
		fis, err = f.listFileInfos(ctx, fd, func(entry os.DirEntry) os.FileInfo {
			newRemote := f.cleanRemote(dir, entry.Name())
			// Skip excluded files
			if entry.Type().IsRegular() && useFilter && !filter.IncludeRemote(newRemote) {
				return nil
			}
			namepath := join.FilePathJoin(fsDirPath, entry.Name())
			fi, fierr := os.Lstat(namepath)
			if os.IsNotExist(fierr) {
				// Skip entry removed by a concurrent goroutine
				return nil
			}
			if fierr != nil {
				// Don't report errors on any file names that are excluded
				if useFilter && !filter.IncludeRemote(newRemote) {
					return nil
				}
				fierr = fmt.Errorf("failed to get info about directory entry %q: %w", namepath, fierr)
				fs.Errorf(dir, "%v", fierr)
				_ = accounting.Stats(ctx).Error(fserrors.NoRetryError(fierr)) // fail the sync
				return nil
			}
			return fi
		})
		// If the error is a timeout or deadline exceeded, close the current fd and set it to nil,
		// so we can reopen the directory
		if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
			fs.Errorf(dir, "Timeout reading directory, reopening and retrying...")
			closeDir()
			continue
		}
		if err == io.EOF && len(fis) == 0 {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read directory entry %q: %w", dir, err)
		}

		// TODO: Parallelise this loop
		for _, fi := range fis {
			name := fi.Name()
			mode := fi.Mode()
			newRemote := f.cleanRemote(dir, name)
			symlinkFlag := os.ModeSymlink
			if runtime.GOOS == "windows" {
				symlinkFlag |= os.ModeIrregular
			}
			// Follow symlinks if required
			if f.opt.FollowSymlinks && (mode&symlinkFlag) != 0 {
				localPath := join.FilePathJoin(fsDirPath, name)
				fi, err = os.Stat(localPath)
				// Quietly skip errors on excluded files and directories
				if err != nil && useFilter && !filter.IncludeRemote(newRemote) {
					continue
				}
				if os.IsNotExist(err) || isCircularSymlinkError(err) {
					// Skip bad symlinks and circular symlinks
					err = fserrors.NoRetryError(fmt.Errorf("symlink: %w", err))
					fs.Errorf(newRemote, "Listing error: %v", err)
					err = accounting.Stats(ctx).Error(err)
					continue
				}
				if err != nil {
					return nil, err
				}
				mode = fi.Mode()
			}
			if fi.IsDir() {
				// Ignore directories which are symlinks. These are junction points under Windows, which
				// are kind of a souped up symlink. Unix doesn't have directories which are symlinks.
				if (mode&symlinkFlag) == 0 && f.dev == readDevice(fi, f.opt.OneFileSystem) {
					d := f.newDirectory(newRemote, fi)
					entries = append(entries, d)
				}
			} else {
				// Check whether this link should be translated
				if f.opt.TranslateSymlinks && fi.Mode()&symlinkFlag != 0 {
					newRemote += fs.LinkSuffix
				}
				// Don't include non directory if not included
				// we leave directory filtering to the layer above
				if useFilter && !filter.IncludeRemote(newRemote) {
					continue
				}
				fso, err := f.newObjectWithInfo(newRemote, fi)
				if err != nil {
					return nil, err
				}
				if fso.Storable() {
					entries = append(entries, fso)
				}
			}
		}
	}

	return entries, nil
}
