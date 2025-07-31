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
	statTimeout = 120
)

// withTimeout runs an operation with timeout and panic protection
func (f *Fs) withTimeout(ctx context.Context, operation func() error) error {
	// Create timeout context
	timeoutCtx, cancel := context.WithTimeout(ctx, statTimeout*time.Second)
	defer cancel()

	done := make(chan error, 1)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				var panicErr error
				if e, ok := r.(error); ok {
					panicErr = fmt.Errorf("panic during async operation: %w", e)
				} else {
					panicErr = fmt.Errorf("panic during async operation: %v", r)
				}
				select {
				case done <- panicErr:
				case <-timeoutCtx.Done():
				}
			}
		}()

		done <- operation()
	}()

	select {
	case err := <-done:
		return err
	case <-timeoutCtx.Done():
		return fmt.Errorf("timeout during async operation: %w", timeoutCtx.Err())
	}
}

// withTimeoutRetry runs an operation with timeout, panic protection, and infinite retry
// Uses exponential backoff up to 15 mins, then continues indefinitely
func (f *Fs) withTimeoutRetry(ctx context.Context, name string, operation func() error) error {
	const maxBackoff = 15 * time.Minute
	backoff := 1 * time.Second
	attempt := 0

	for {
		attempt++
		err := f.withTimeout(ctx, operation)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				// Check for context cancellation
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
				}

				fs.Errorf(name, "Timeout during stat operation (attempt %d), retrying in %v", attempt, backoff)

				// Sleep with context cancellation support
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
					return ctx.Err()
				}

				// Exponential backoff up to maxBackoff, then stay at maxBackoff
				if backoff < maxBackoff {
					backoff *= 2
					if backoff > maxBackoff {
						backoff = maxBackoff
					}
				}

				continue
			}
			return err
		}
		return nil
	}
}

// asyncLstat performs a single lstat operation with timeout protection using f.lstat
func (f *Fs) asyncLstat(ctx context.Context, path string) (os.FileInfo, error) {
	var result struct {
		fi  os.FileInfo
		err error
	}

	timeoutErr := f.withTimeoutRetry(ctx, path, func() error {
		result.fi, result.err = f.lstat(path)
		return nil
	})
	if timeoutErr != nil {
		return nil, timeoutErr
	}

	return result.fi, result.err
}

// asyncStat performs a single stat operation with timeout protection
func (f *Fs) asyncStat(ctx context.Context, path string) (os.FileInfo, error) {
	var result struct {
		fi  os.FileInfo
		err error
	}

	timeoutErr := f.withTimeoutRetry(ctx, path, func() error {
		result.fi, result.err = os.Stat(path)
		return nil
	})
	if timeoutErr != nil {
		return nil, timeoutErr
	}

	return result.fi, result.err
}

// asyncStatEntries performs stat operations on directory entries with timeout protection
func (f *Fs) asyncStatEntries(ctx context.Context, entries []os.DirEntry, statFunc func(entry os.DirEntry) os.FileInfo) ([]os.FileInfo, error) {
	var fis []os.FileInfo

	for _, entry := range entries {
		var fi os.FileInfo

		timeoutErr := f.withTimeoutRetry(ctx, entry.Name(), func() error {
			fi = statFunc(entry)
			return nil
		})
		if timeoutErr != nil {
			return nil, timeoutErr
		}

		if fi != nil {
			fis = append(fis, fi)
		}
	}

	return fis, nil
}

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

	// Set read deadline if supported
	deadline := time.Now().Add(statTimeout * time.Second)
	err = fd.SetReadDeadline(deadline)
	if err == nil || !errors.Is(err, os.ErrNoDeadline) {
		defer func() {
			_ = fd.SetReadDeadline(time.Time{})
		}()
	}

	if useReadDir {
		// Windows and Plan9 read the directory entries with the stat information
		var result struct {
			fis []os.FileInfo
			err error
		}
		timeoutErr := f.withTimeout(ctx, func() error {
			result.fis, result.err = fd.Readdir(1024)
			return nil
		})
		if timeoutErr != nil {
			return nil, timeoutErr
		}
		return result.fis, result.err
	}

	// For other OSes we read the names only then stat individually
	var result struct {
		entries []os.DirEntry
		err     error
	}
	timeoutErr := f.withTimeout(ctx, func() error {
		result.entries, result.err = fd.ReadDir(1024)
		return nil
	})
	if timeoutErr != nil {
		return nil, timeoutErr
	}
	if result.err == nil {
		// TODO: Parallelise this loop
		return f.asyncStatEntries(ctx, result.entries, statFunc)
	}
	return nil, result.err
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

	fsDirPath, err := f.localPath(dir)
	if err != nil {
		return nil, err
	}
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
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
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

		fdStat, err := fd.Stat()
		if err != nil {
			return nil, fmt.Errorf("failed to stat directory %q: %w", dir, err)
		}
		fdModTime := fdStat.ModTime()

		timeNow := time.Now()
		directoryRecentlyChanged := !fdModTime.IsZero() && !fdModTime.After(timeNow.Add(1*time.Hour)) &&
			fdModTime.Add(3*time.Hour).After(timeNow)

		var fis []os.FileInfo

		fis, err = f.listFileInfos(ctx, fd, func(entry os.DirEntry) os.FileInfo {
			newRemote := f.cleanRemote(dir, entry.Name())
			// Skip excluded files
			if entry.Type().IsRegular() && useFilter && !filter.IncludeRemote(newRemote) {
				return nil
			}
			namepath := join.FilePathJoin(fsDirPath, entry.Name())
			fi, fierr := entry.Info()
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

			// Skip files created in the last 5 minutes, if the option is enabled
			// This is to avoid listing files that are being created or modified
			// Only perform the check if the time is not zero and not in the future
			if entry.Type().IsRegular() && f.opt.SkipRecent {
				// Check if there's been recent directory activity (creates/deletes/renames)
				// Directory modtime is more reliable than file modtime since file modtime can be manipulated
				if directoryRecentlyChanged {
					// Directory had recent activity, check if this specific file was recently updated
					fileCTime := readTime(cTime, fi)
					fileRecentlyChanged := !fileCTime.IsZero() &&
						!fileCTime.After(timeNow.Add(1*time.Hour)) &&
						fileCTime.Add(5*time.Minute).After(timeNow)

					if fileRecentlyChanged {
						return nil // Skip recently updated files when directory shows recent activity
					}
				}
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
				fi, err = f.asyncStat(ctx, localPath)
				// Quietly skip errors on excluded files and directories
				if err != nil && useFilter && !filter.IncludeRemote(newRemote) {
					continue
				}
				if os.IsNotExist(err) || isCircularSymlinkError(err) {
					// Skip bad symlinks and circular symlinks
					err = fserrors.NoRetryError(fmt.Errorf("symlink: %w", err))
					fs.Errorf(newRemote, "Symlink listing error: %v", err)
					_ = accounting.Stats(ctx).Error(err)
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
					d, err := f.newDirectory(newRemote, fi)
					if err != nil {
						return nil, err
					}
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
