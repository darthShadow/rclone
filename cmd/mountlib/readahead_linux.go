//go:build linux

package mountlib

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rclone/rclone/fs"
	"golang.org/x/sys/unix"
)

const (
	// Maximum time to wait for sysfs path to appear
	sysfsTimeout = 1 * time.Second
	// Polling interval when waiting for sysfs
	sysfsPollInterval = 50 * time.Millisecond
	// MinReadAheadKiB is the minimum readahead value (kernel default)
	MinReadAheadKiB = 128
	// DefaultMaxWrite is the kernel's default max FUSE write size (1 MiB).
	// This has been the limit since Linux 4.20 (256 pages × 4 KiB).
	// Kernel 6.13+ allows tuning via /proc/sys/fs/fuse/max_pages_limit.
	DefaultMaxWrite = 1024 * 1024
	// procMaxPagesLimit is the procfs path for the tunable max pages (kernel 6.13+)
	procMaxPagesLimit = "/proc/sys/fs/fuse/max_pages_limit"
)

// EnforceMinReadAheadKiB returns sizeKiB with minimum enforcement applied.
// Values below MinReadAheadKiB (128 KiB) are raised to MinReadAheadKiB
// to avoid accidentally degrading performance. Zero and negative values
// are normalized to zero (meaning "use default").
func EnforceMinReadAheadKiB(sizeKiB int) int {
	if sizeKiB <= 0 {
		return 0
	}
	if sizeKiB < MinReadAheadKiB {
		return MinReadAheadKiB
	}
	return sizeKiB
}

// TuneKernelReadAhead sets the kernel readahead for the FUSE mount.
// This must be called after the mount is ready.
// sizeKiB is the desired readahead in KiB. Values below 128 KiB are
// raised to 128 KiB (the kernel default) to avoid degraded performance.
// Returns error if tuning fails (caller may choose to log and continue).
func TuneKernelReadAhead(mountpoint string, sizeKiB int) error {
	sizeKiB = EnforceMinReadAheadKiB(sizeKiB)

	var st syscall.Stat_t
	if err := syscall.Stat(mountpoint, &st); err != nil {
		return fmt.Errorf("stat mountpoint %q: %w", mountpoint, err)
	}

	major := unix.Major(st.Dev)
	minor := unix.Minor(st.Dev)
	sysfsPath := fmt.Sprintf("/sys/class/bdi/%d:%d/read_ahead_kb", major, minor)

	// Poll for sysfs path to appear (may take a moment after mount)
	deadline := time.Now().Add(sysfsTimeout)
	for {
		if _, err := os.Stat(sysfsPath); err == nil {
			break // Path exists
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("sysfs path %q did not appear within %v", sysfsPath, sysfsTimeout)
		}
		time.Sleep(sysfsPollInterval)
	}

	// Write the value
	if err := os.WriteFile(sysfsPath, []byte(strconv.Itoa(sizeKiB)), 0644); err != nil {
		return fmt.Errorf("write to %q: %w", sysfsPath, err)
	}

	fs.LogLevelPrintf(fs.LogLevelNotice, nil, "Set kernel readahead to %d KiB for %q", sizeKiB, mountpoint)
	return nil
}

// GetKernelReadAhead returns the current kernel readahead for the mountpoint in KiB.
func GetKernelReadAhead(mountpoint string) (int, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(mountpoint, &st); err != nil {
		return 0, err
	}

	major := unix.Major(st.Dev)
	minor := unix.Minor(st.Dev)
	sysfsPath := fmt.Sprintf("/sys/class/bdi/%d:%d/read_ahead_kb", major, minor)

	data, err := os.ReadFile(sysfsPath)
	if err != nil {
		return 0, err
	}

	return strconv.Atoi(strings.TrimSpace(string(data)))
}

// GetEffectiveMaxWrite returns the kernel's effective maximum FUSE write size.
// On Linux 6.13+, this reads /proc/sys/fs/fuse/max_pages_limit and calculates
// the limit as max_pages × page_size. On older kernels where the procfs tunable
// doesn't exist, returns DefaultMaxWrite (1 MiB).
func GetEffectiveMaxWrite() int {
	data, err := os.ReadFile(procMaxPagesLimit)
	if err != nil {
		// Pre-6.13 kernel or procfs not mounted
		return DefaultMaxWrite
	}

	maxPages, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || maxPages <= 0 {
		return DefaultMaxWrite
	}

	pageSize := os.Getpagesize()
	return maxPages * pageSize
}
