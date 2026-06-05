//go:build linux

package local

import (
	"context"
	"fmt"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/lib/file"
	"golang.org/x/sys/unix"
)

// Fd returns a caller-owned file descriptor for the Object.
//
// The returned fd is a fresh dup produced by F_DUPFD_CLOEXEC so it is both
// independent of the internally opened file (which is closed before return)
// and carries the close-on-exec flag. A non-zero return value with a nil
// error is a valid, open fd; the caller MUST close it exactly once. If closing
// the internally opened file fails after a successful dup, Fd closes the dup
// and returns the close error instead of leaking a usable fd to the caller.
// Returns (0, fs.ErrorNotImplemented) on platforms where this file is not
// compiled (see fd_unsupported.go).
//
// flags is passed directly to file.OpenFile and controls the open mode
// (e.g. os.O_RDONLY).
func (o *Object) Fd(ctx context.Context, flags int) (uintptr, error) {
	osFile, err := file.OpenFile(o.path, flags, 0)
	if err != nil {
		return 0, fmt.Errorf("failed to open file for dup: %s: %w", o.path, err)
	}
	// F_DUPFD_CLOEXEC atomically dups the fd and sets O_CLOEXEC, avoiding
	// a race window between Dup and a separate FD_CLOEXEC fcntl call. The
	// third argument is the minimum fd number; 3 skips stdin/stdout/stderr.
	// A non-zero result guarantees the dup is not the fd=0 sentinel.
	dupFd, errno := unix.FcntlInt(osFile.Fd(), unix.F_DUPFD_CLOEXEC, 3)
	if errno != nil {
		_ = osFile.Close()
		return 0, fmt.Errorf("F_DUPFD_CLOEXEC failed for %s: %w", o.path, errno)
	}
	if err := osFile.Close(); err != nil {
		_ = unix.Close(dupFd)
		return 0, fmt.Errorf("failed to close file after dup: %s: %w", o.path, err)
	}
	fs.Infof(o, "Returning dup fd: %s: %d", o.path, dupFd)
	return uintptr(dupFd), nil
}
