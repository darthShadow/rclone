// Device reading functions

//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package local

import (
	"os"
	"syscall"

	"github.com/rclone/rclone/fs"
)

// readDevice turns a valid os.FileInfo into a device number,
// returning devUnset if it fails.
func readDevice(fi os.FileInfo, oneFileSystem bool) uint64 {
	if !oneFileSystem {
		return devUnset
	}
	statT, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		fs.Debugf(fi.Name(), "Type assertion fi.Sys().(*syscall.Stat_t) failed from: %#v", fi.Sys())
		return devUnset
	}
	return uint64(statT.Dev) // nolint: unconvert
}

// readIdentity returns the device and inode identity, or 0 if unavailable.
func readIdentity(fi os.FileInfo) uint64 {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return localIdentity(uint64(st.Dev), uint64(st.Ino)) // nolint: unconvert
}

func localIdentity(dev, ino uint64) uint64 {
	if ino == 0 {
		return 0
	}
	id := ino ^ dev*0x9e3779b97f4a7c15
	if id == 0 {
		return 1
	}
	return id
}
