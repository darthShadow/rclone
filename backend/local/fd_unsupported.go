//go:build !linux

package local

import (
	"context"

	"github.com/rclone/rclone/fs"
)

// Fd is not supported on this platform. Returns (0, fs.ErrorNotImplemented)
// without opening any file. Callers must gracefully disable passthrough for
// the affected handle when this error is returned.
func (o *Object) Fd(ctx context.Context, flags int) (uintptr, error) {
	return 0, fs.ErrorNotImplemented
}
