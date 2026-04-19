//go:build !(linux && amd64)

// This file is the fallback for all platforms other than linux/amd64. The
// sequential listing loop still reads one ReadDir batch at a time, but there is
// no batch-scoped fstatat helper on these targets, so statDirEntry delegates to
// entry.Info().

package local

import "os"

// openDirAtReadFD is a no-op stub: this platform has no batch-scoped dirfd fast
// path, so it returns an invalid raw fd. The caller detects that and falls back
// to entry.Info().
func openDirAtReadFD(fd *os.File) (int, error) {
	return invalidStatDirFD, nil
}

// listStatDirFD reports that there is no raw dirfd to reuse on this platform.
func listStatDirFD(fd int) (int, bool) {
	return invalidStatDirFD, false
}

// statDirEntry falls back to the cached DirEntry.Info implementation.
func statDirEntry(entry *cachedDirEntry, nameBuf []byte) (os.FileInfo, []byte, error) {
	fi, err := entry.Info()
	return fi, nameBuf, err
}
