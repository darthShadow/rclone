//go:build !linux

package mountlib

import (
	"errors"

	"github.com/rclone/rclone/fs"
)

// MinReadAheadKiB is the minimum readahead value (kernel default).
// On non-Linux platforms this is defined for API compatibility but not enforced.
const MinReadAheadKiB = 128

// DefaultMaxWrite is the kernel's default max FUSE write size (1 MiB).
// On non-Linux platforms this is defined for API compatibility.
const DefaultMaxWrite = 1024 * 1024

// EnforceMinReadAheadKiB returns sizeKiB unchanged on non-Linux platforms.
// Minimum enforcement only applies on Linux where kernel sysfs tuning is supported.
func EnforceMinReadAheadKiB(sizeKiB int) int {
	return sizeKiB
}

// TuneKernelReadAhead sets the kernel readahead for the FUSE mount.
// On non-Linux platforms this is a no-op that returns nil.
func TuneKernelReadAhead(mountpoint string, sizeKiB int) error {
	fs.Debugf(nil, "readahead: kernel tuning not supported on this platform")
	return nil // Not an error, just unsupported
}

// GetKernelReadAhead returns the current kernel readahead for the mountpoint in KiB.
// On non-Linux platforms this returns an error.
func GetKernelReadAhead(mountpoint string) (int, error) {
	return 0, errors.New("not supported on this platform")
}

// GetEffectiveMaxWrite returns the kernel's effective maximum FUSE write size.
// On non-Linux platforms this returns DefaultMaxWrite (1 MiB) as we cannot
// detect kernel limits.
func GetEffectiveMaxWrite() int {
	return DefaultMaxWrite
}
