package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
	"github.com/rclone/rclone/fs/filter"
	"github.com/rclone/rclone/fs/fserrors"
)

// watchdogInterval <= stuckWarnTimeout <= statTimeout <= stuckReplaceTimeout is
// one coupled scheduler policy. Retuning one constant in isolation changes
// recovery ordering and can cause premature replacement or very slow recovery.
const (
	statTimeout          = 15 // seconds - used as statTimeout * time.Second
	watchdogInterval     = 5 * time.Second
	stuckWarnTimeout     = 10 * time.Second
	stuckReplaceTimeout  = 45 * time.Second
	listReadDirBatchSize = 1024
)

// cachedDirEntry is filled by the listing flow's single writer before any stat
// worker observes it. That structural invariant keeps cached remote/stat-dir
// state race-free without extra synchronization.
type cachedDirEntry struct {
	os.DirEntry
	remote    string
	statDirFD int
	useStatFD bool
}

// Remote returns the cached remote path.
func (entry *cachedDirEntry) Remote() string {
	return entry.remote
}

func newCachedDirEntry(entry os.DirEntry, owner *Fs, remotePrefix string) cachedDirEntry {
	cached := cachedDirEntry{DirEntry: entry}
	if owner != nil {
		cached.remote = owner.cleanRemoteWithPrefix(remotePrefix, entry.Name())
	}
	return cached
}

type fileInfoDirEntry struct {
	os.FileInfo
}

func (entry fileInfoDirEntry) Type() os.FileMode { return entry.Mode().Type() }
func (entry fileInfoDirEntry) Info() (os.FileInfo, error) {
	return entry.FileInfo, nil
}

type listBatchConsumer func(entries []cachedDirEntry, fis []statFileInfo) error

const invalidStatDirFD = -1

type readResult struct {
	entries []os.DirEntry
	err     error
	statDir int
}

func newReadResult() readResult {
	return readResult{statDir: invalidStatDirFD}
}

func closeStatDirFD(fd *int) {
	if fd == nil || *fd < 0 {
		return
	}
	_ = syscall.Close(*fd)
	*fd = invalidStatDirFD
}

// closeReadResultStatDir releases the batch-scoped stat fd when a ReadDir
// batch exits before handing ownership to a batchController.
func closeReadResultStatDir(batch *readResult) {
	closeStatDirFD(&batch.statDir)
}

func stopTimer(timer *time.Timer) {
	if timer == nil {
		return
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

// listCachedFileInfos reads one directory. The first ReadDir(listReadDirBatchSize)
// batch stays inline so single-batch directories avoid prefetch overhead; if that
// batch does not finish the directory, later batches are read through
// prefetchReader so ReadDir for batch N+1 overlaps stat work for batch N.
// dirRemotePrefix must be empty when listing the root and
// must be the directory remote prefix when listing a nested directory so
// newCachedDirEntry produces correct full paths. Internal callers that
// guarantee neither consume nor statFunc calls Remote may pass empty
// unconditionally. The consume callback must skip result slots whose fi is nil
// because batch results are no longer compacted. Batch slices passed to consume
// are valid only for the duration of that callback.
func (f *Fs) listCachedFileInfos(ctx context.Context, fd *os.File, openDir func() (*os.File, error), dirRemotePrefix string, preFilter func(os.DirEntry) (cachedDirEntry, bool), statFunc func(entry *cachedDirEntry, nameBuf []byte) (os.FileInfo, []byte, error), consume listBatchConsumer) (err error) {
	defer func() {
		if r := recover(); r != nil {
			var panicErr error
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

	if useReadDir {
		// Windows/Plan9 still use a sequential Readdir loop, but Readdir already
		// returns os.FileInfo so there is no separate batch stat phase. This path
		// does not apply preFilter or statFunc, so IncludeDirectory and SkipRecent
		// are not enforced here. That is pre-existing behavior, not a regression.
		var allFis []statFileInfo
		currentFd := fd
		defer func() {
			if currentFd != nil {
				_ = currentFd.Close()
			}
		}()
		for {
			var result struct {
				fis []os.FileInfo
				err error
			}
			readFd := currentFd
			if sErr := readFd.SetReadDeadline(time.Now().Add(statTimeout * time.Second)); sErr != nil && !errors.Is(sErr, os.ErrNoDeadline) {
				fs.Debugf(f, "SetReadDeadline: %v", sErr)
			}
			result.fis, result.err = readFd.Readdir(listReadDirBatchSize)
			if !errors.Is(result.err, os.ErrDeadlineExceeded) {
				if len(result.fis) > 0 {
					for _, fi := range result.fis {
						allFis = append(allFis, newStatFileInfoFromOS(fi))
					}
				}
				if result.err == io.EOF {
					break
				}
				if result.err != nil {
					return result.err
				}
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			fs.Errorf(f, "Timeout reading directory, reopening and retrying...")
			_ = readFd.Close()
			currentFd = nil
			newFd, openErr := openDir()
			if openErr != nil {
				return openErr
			}
			currentFd = newFd
			if sErr := currentFd.SetReadDeadline(time.Now().Add(statTimeout * time.Second)); sErr != nil && !errors.Is(sErr, os.ErrNoDeadline) {
				fs.Debugf(f, "SetReadDeadline on reopened fd: %v", sErr)
			}
			allFis = allFis[:0]
			continue
		}
		if consume == nil || len(allFis) == 0 {
			return nil
		}
		entries := make([]cachedDirEntry, len(allFis))
		for i := range allFis {
			entries[i] = newCachedDirEntry(fileInfoDirEntry{FileInfo: allFis[i].fi}, f, dirRemotePrefix)
		}
		return consume(entries, allFis)
	}

	// TODO(adversarial-review): local-main had no process-wide listing admission
	// control at all. The scheduler worker pool here is already strictly better
	// because stuck stats must pass through shared admission and replacement
	// instead of spawning directly from every listing. We still do not add a
	// process-wide breaker in this round; revisit if io_uring does not land and
	// sustained stuck-CephFS outages show up in the field.
	controller := newListController(f, f.statScheduler, listControllerOptions{})
	if len(dirRemotePrefix) > 0 {
		controller.dirPath = strings.TrimSuffix(dirRemotePrefix, "/")
	}
	fdOwned := true
	defer func() {
		if fdOwned && fd != nil {
			_ = fd.Close()
		}
	}()

	if err := ctx.Err(); err != nil {
		return err
	}

	batch := newReadResult()
	// TODO: cancel support could be restored via per-call fd dup
	// (F_DUPFD_CLOEXEC) so the cancel arm closes only the dup, leaving the
	// original fd untouched. Current synchronous form blocks indefinitely on
	// stuck CephFS readdir; that matches upstream and is acceptable for current
	// workloads.
	batch.entries, batch.err = fd.ReadDir(listReadDirBatchSize)
	// The one-entry probe proves EOF on the first short batch so small
	// directories stay on the synchronous hot path instead of paying a prefetch
	// goroutine plus fd handoff for the final read.
	if batch.err == nil && len(batch.entries) < listReadDirBatchSize {
		probeEntries, probeErr := fd.ReadDir(1)
		switch probeErr {
		case nil:
			batch.entries = append(batch.entries, probeEntries...)
		case io.EOF:
			batch.err = io.EOF
		default:
			batch.err = probeErr
		}
	}
	if len(batch.entries) > 0 {
		// Give this ReadDir batch its own stat handle tied to the same opened
		// directory so fstatat work can reuse one batch-scoped dirfd.
		statDir, openErr := openDirAtReadFD(fd)
		if openErr != nil {
			fs.Debugf(f, "openDirAtReadFD: %v, falling back to entry.Info()", openErr)
		} else {
			batch.statDir = statDir
		}
	}

	err = controller.ProcessBatch(ctx, &batch, preFilter, statFunc, consume)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if batch.err == io.EOF {
		return nil
	}
	if batch.err != nil {
		return batch.err
	}

	reader := newListPrefetchReader(ctx, f, fd)
	fdOwned = false
	defer reader.Close()

	for {
		batch, ok := reader.Next(ctx)
		if !ok {
			if err := ctx.Err(); err != nil {
				return err
			}
			return nil
		}

		err = controller.ProcessBatch(ctx, &batch, preFilter, statFunc, consume)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if batch.err == io.EOF {
			break
		}
		if batch.err != nil {
			return batch.err
		}
	}

	return nil
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
	// Skip stat for filtered directories when no exclude-if-present files are
	// configured (IncludeDirectory does I/O for exclude-file checks).
	var includeDirFn func(string) (bool, error)
	if useFilter && len(filter.Opt.ExcludeFile) == 0 {
		includeDirFn = filter.IncludeDirectory(ctx, f)
	}

	fsDirPath, err := f.localPath(dir)
	if err != nil {
		return nil, err
	}
	dirRemotePrefix := ""
	if dir != "" {
		dirRemotePrefix = dir + "/"
	}
	listingLocalPrefix := localPathPrefix(fsDirPath)
	_, err = os.Stat(fsDirPath)
	if err != nil {
		return nil, fs.ErrorDirNotFound
	}

	var fd *os.File
	openDir := func() (*os.File, error) {
		return os.Open(fsDirPath)
	}

	fd, err = openDir()
	if err != nil {
		isPerm := os.IsPermission(err)
		err = fmt.Errorf("failed to open directory %q: %w", dir, err)
		fs.Errorf(dir, "%v", err)
		if isPerm {
			_ = accounting.Stats(ctx).Error(fserrors.NoRetryError(err))
			err = nil
		}
		return nil, err
	}

	fdStat, err := fd.Stat()
	if err != nil {
		_ = fd.Close()
		return nil, fmt.Errorf("failed to stat directory %q: %w", dir, err)
	}
	fdModTime := fdStat.ModTime()

	timeNow := time.Now()
	directoryRecentlyChanged := !fdModTime.IsZero() && !fdModTime.After(timeNow.Add(1*time.Hour)) &&
		fdModTime.Add(3*time.Hour).After(timeNow)

	preFilter := func(entry os.DirEntry) (cachedDirEntry, bool) {
		cachedEntry := newCachedDirEntry(entry, f, dirRemotePrefix)
		if !useFilter {
			return cachedEntry, true
		}
		if entry.Type() != 0 {
			return cachedEntry, true
		}
		if filter.IncludeRemote(cachedEntry.Remote()) {
			return cachedEntry, true
		}
		return cachedEntry, slices.Contains(filter.Opt.ExcludeFile, entry.Name())
	}

	statFunc := func(entry *cachedDirEntry, nameBuf []byte) (os.FileInfo, []byte, error) {
		entryType := entry.Type()
		if includeDirFn != nil && entryType.IsDir() {
			newRemote := entry.Remote()
			include, inclErr := includeDirFn(newRemote)
			if inclErr != nil {
				fs.Infof(newRemote, "directory exclusion check failed: %v", inclErr)
			}
			if !include {
				return nil, nameBuf, nil
			}
		}
		fi, nextNameBuf, fierr := statDirEntry(entry, nameBuf)
		if os.IsNotExist(fierr) {
			return nil, nextNameBuf, nil
		}
		if fierr != nil {
			if useFilter && !filter.IncludeRemote(entry.Remote()) {
				return nil, nextNameBuf, nil
			}
			namepath := listingLocalPrefix + entry.Name()
			fierr = fmt.Errorf("failed to get info about directory entry %q: %w", namepath, fierr)
			fs.Errorf(dir, "%v", fierr)
			_ = accounting.Stats(ctx).Error(fserrors.NoRetryError(fierr))
			return nil, nextNameBuf, nil
		}
		if entryType == 0 && f.opt.SkipRecent {
			if directoryRecentlyChanged {
				fileCTime := readTime(cTime, fi)
				fileRecentlyChanged := !fileCTime.IsZero() &&
					!fileCTime.After(timeNow.Add(1*time.Hour)) &&
					fileCTime.Add(5*time.Minute).After(timeNow)
				if fileRecentlyChanged {
					return nil, nextNameBuf, nil
				}
			}
		}

		return fi, nextNameBuf, nil
	}
	entries = fs.DirEntries{}
	err = f.listCachedFileInfos(ctx, fd, openDir, dirRemotePrefix, preFilter, statFunc, func(batchEntries []cachedDirEntry, fis []statFileInfo) error {
		for i := range fis {
			fi := fis[i].fi
			if fi == nil {
				continue
			}
			entry := &batchEntries[i]
			name := entry.Name()
			localPath := listingLocalPrefix + name
			mode := fi.Mode()
			newRemote := entry.Remote()
			symlinkFlag := os.ModeSymlink
			if runtime.GOOS == "windows" {
				symlinkFlag |= os.ModeIrregular
			}
			if f.opt.FollowSymlinks && (mode&symlinkFlag) != 0 {
				fi, err = f.statScheduler.SubmitOne(ctx, localPath, false)
				if err != nil && useFilter && !filter.IncludeRemote(newRemote) {
					continue
				}
				if os.IsNotExist(err) || isCircularSymlinkError(err) {
					err = fserrors.NoRetryError(fmt.Errorf("symlink: %w", err))
					fs.Errorf(newRemote, "Symlink listing error: %v", err)
					_ = accounting.Stats(ctx).Error(err)
					continue
				}
				if err != nil {
					return err
				}
				mode = fi.Mode()
			}
			if fi.IsDir() {
				if (mode&symlinkFlag) == 0 && f.dev == readDevice(fi, f.opt.OneFileSystem) {
					d := f.newListedDirectory(newRemote, localPath, fi)
					entries = append(entries, d)
				}
				continue
			}
			translatedLink := f.opt.TranslateSymlinks && fi.Mode()&symlinkFlag != 0
			if translatedLink {
				newRemote += fs.LinkSuffix
			}
			if useFilter && !filter.IncludeRemote(newRemote) &&
				!slices.Contains(filter.Opt.ExcludeFile, name) {
				continue
			}
			fso, objErr := f.newListedObject(newRemote, localPath, translatedLink, fi)
			if objErr != nil {
				return objErr
			}
			if fso.Storable() {
				entries = append(entries, fso)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to read directory entry %q: %w", dir, err)
	}
	// Uncomment to signal pre-filtered entries:
	// FIXME: enable SetPreFiltered once preFilter in List correctly excludes all entries that IncludeRemote would reject
	return entries, nil
}
