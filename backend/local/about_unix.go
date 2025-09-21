//go:build darwin || dragonfly || freebsd || linux

package local

import (
	"context"
	"fmt"
	"math"
	"os"

	"github.com/rclone/rclone/fs"
	"golang.org/x/sys/unix"
)

// calculateOptimalBlockSize determines the optimal block size for I/O operations
// by examining both statfs and stat block sizes and applying bounds checking
func calculateOptimalBlockSize(statfsBlockSize, statBlockSize int64) int32 {
	const defaultBlockSize = 4096      // reasonable default for modern filesystems
	const maxBlockSize = math.MaxInt32 // max int32 value (~2GB)

	// Apply bounds checking for statfs block size
	if statfsBlockSize <= 0 {
		statfsBlockSize = defaultBlockSize
	} else if statfsBlockSize > maxBlockSize {
		statfsBlockSize = maxBlockSize
	}

	// Apply bounds checking for stat block size
	if statBlockSize <= 0 {
		statBlockSize = defaultBlockSize
	} else if statBlockSize > maxBlockSize {
		statBlockSize = maxBlockSize
	}

	// Use minimum of both block sizes for optimal IO
	return int32(min(statfsBlockSize, statBlockSize))
}

// About gets quota information
func (f *Fs) About(ctx context.Context) (*fs.Usage, error) {
	var unixStatfs unix.Statfs_t
	err := unix.Statfs(f.root, &unixStatfs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fs.ErrorDirNotFound
		}
		return nil, fmt.Errorf("failed to read disk usage: %w", err)
	}

	// Get statfs block size (filesystem block size)
	statfsBlockSize := int64(unixStatfs.Bsize) // nolint: unconvert

	// Get stat block size from a file in the directory (preferred I/O block size)
	var statBlockSize int64
	var unixStat unix.Stat_t
	err = unix.Stat(f.root, &unixStat)
	if err == nil {
		statBlockSize = int64(unixStat.Blksize) // nolint: unconvert
	}

	// Calculate optimal block size with bounds checking
	bs := calculateOptimalBlockSize(statfsBlockSize, statBlockSize)

	usage := &fs.Usage{
		Total:       fs.NewUsageValue(statfsBlockSize * int64(unixStatfs.Blocks)),                  //nolint: unconvert // quota of bytes that can be used
		Used:        fs.NewUsageValue(statfsBlockSize * int64(unixStatfs.Blocks-unixStatfs.Bfree)), //nolint: unconvert // bytes in use
		Free:        fs.NewUsageValue(statfsBlockSize * int64(unixStatfs.Bavail)),                  //nolint: unconvert // bytes which can be uploaded before reaching the quota
		IOBlockSize: fs.NewUsageValue32(bs),                                                        // preferred IO block size for this filesystem
	}
	return usage, nil
}

// check interface
var _ fs.Abouter = &Fs{}
