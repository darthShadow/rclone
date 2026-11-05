// Device reading functions

//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package local

import "os"

// readDevice turns a valid os.FileInfo into a device number,
// returning devUnset if it fails.
func readDevice(fi os.FileInfo, oneFileSystem bool) uint64 {
	return devUnset
}

// readIdentity returns 0 because entry identity is unavailable on this platform.
func readIdentity(fi os.FileInfo) uint64 {
	return 0
}
