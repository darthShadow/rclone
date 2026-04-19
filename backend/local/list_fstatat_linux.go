//go:build linux && amd64

// This file implements the linux/amd64 fast-stat path used by the sequential
// listing loop. openDirAtReadFD opens "." relative to the listing read fd to
// create a batch-scoped raw stat dirfd, listStatDirFD validates that raw fd
// once per batch, and statDirEntry uses fstatat(dirfd, name,
// AT_SYMLINK_NOFOLLOW) without constructing absolute paths. Other platforms use
// list_fstatat_other.go and fall back to entry.Info().

package local

import (
	"os"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

type fstatatFileInfo struct {
	name    string
	mode    os.FileMode
	size    int64
	modTime time.Time
	stat    syscall.Stat_t
}

func (fi *fstatatFileInfo) Name() string       { return fi.name }
func (fi *fstatatFileInfo) Size() int64        { return fi.size }
func (fi *fstatatFileInfo) Mode() os.FileMode  { return fi.mode }
func (fi *fstatatFileInfo) ModTime() time.Time { return fi.modTime }
func (fi *fstatatFileInfo) IsDir() bool        { return fi.mode.IsDir() }
func (fi *fstatatFileInfo) Sys() any           { return &fi.stat }

func fileModeFromStat(m uint32) os.FileMode {
	mode := os.FileMode(m & 0o777)
	switch m & syscall.S_IFMT {
	case syscall.S_IFDIR:
		mode |= os.ModeDir
	case syscall.S_IFLNK:
		mode |= os.ModeSymlink
	case syscall.S_IFIFO:
		mode |= os.ModeNamedPipe
	case syscall.S_IFSOCK:
		mode |= os.ModeSocket
	case syscall.S_IFBLK:
		mode |= os.ModeDevice
	case syscall.S_IFCHR:
		mode |= os.ModeDevice | os.ModeCharDevice
	case syscall.S_IFREG:
		// regular file, no extra bits
	default:
		mode |= os.ModeIrregular
	}
	if m&syscall.S_ISUID != 0 {
		mode |= os.ModeSetuid
	}
	if m&syscall.S_ISGID != 0 {
		mode |= os.ModeSetgid
	}
	if m&syscall.S_ISVTX != 0 {
		mode |= os.ModeSticky
	}
	return mode
}

func syscallTimespecFromUnix(t unix.Timespec) syscall.Timespec {
	return syscall.Timespec{
		Sec:  t.Sec,
		Nsec: t.Nsec,
	}
}

func syscallStatFromUnix(st unix.Stat_t) syscall.Stat_t {
	return syscall.Stat_t{
		Dev:     st.Dev,
		Ino:     st.Ino,
		Nlink:   st.Nlink,
		Mode:    st.Mode,
		Uid:     st.Uid,
		Gid:     st.Gid,
		Rdev:    st.Rdev,
		Size:    st.Size,
		Blksize: st.Blksize,
		Blocks:  st.Blocks,
		Atim:    syscallTimespecFromUnix(st.Atim),
		Mtim:    syscallTimespecFromUnix(st.Mtim),
		Ctim:    syscallTimespecFromUnix(st.Ctim),
	}
}

func fstatatNoFollow(dirfd int, name string, nameBuf []byte) (os.FileInfo, []byte, error) {
	if strings.IndexByte(name, 0) >= 0 {
		return nil, nameBuf, syscall.EINVAL
	}

	need := len(name) + 1
	if cap(nameBuf) < need {
		nameBuf = make([]byte, need)
	} else {
		nameBuf = nameBuf[:need]
	}
	copy(nameBuf, name)
	nameBuf[len(name)] = 0

	var st unix.Stat_t
	for {
		_, _, errno := unix.Syscall6(
			unix.SYS_NEWFSTATAT,
			uintptr(dirfd),
			uintptr(unsafe.Pointer(&nameBuf[0])),
			uintptr(unsafe.Pointer(&st)),
			uintptr(unix.AT_SYMLINK_NOFOLLOW),
			0,
			0,
		)
		runtime.KeepAlive(nameBuf)
		runtime.KeepAlive(&st)
		if errno == syscall.EINTR {
			continue
		}
		if errno != 0 {
			return nil, nameBuf[:0], errno
		}
		break
	}
	// int64 casts are no-ops on amd64 but required for other architectures if the build tag is widened.
	//nolint:unconvert
	modTime := time.Unix(int64(st.Mtim.Sec), int64(st.Mtim.Nsec))
	return &fstatatFileInfo{
		name:    name,
		mode:    fileModeFromStat(st.Mode),
		size:    st.Size,
		modTime: modTime,
		stat:    syscallStatFromUnix(st),
	}, nameBuf[:0], nil
}

// openDirAtReadFD opens "." relative to the active listing fd so one ReadDir
// batch gets its own directory handle for fstatat work.
func openDirAtReadFD(fd *os.File) (int, error) {
	if fd == nil {
		return invalidStatDirFD, os.ErrClosed
	}

	rawfd := fd.Fd()
	if rawfd == ^uintptr(0) {
		return invalidStatDirFD, os.ErrClosed
	}

	for {
		statFD, err := unix.Openat(int(rawfd), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return invalidStatDirFD, err
		}
		return statFD, nil
	}
}

// listStatDirFD validates the batch-owned raw dirfd before the batch snapshots
// it into each cached entry.
func listStatDirFD(fd int) (int, bool) {
	if fd < 0 {
		return invalidStatDirFD, false
	}
	return fd, true
}

// statDirEntry stats one cached entry through the batch-scoped dirfd when
// available, otherwise it falls back to the DirEntry.Info path.
func statDirEntry(entry *cachedDirEntry, nameBuf []byte) (os.FileInfo, []byte, error) {
	if entry.useStatFD {
		return fstatatNoFollow(entry.statDirFD, entry.Name(), nameBuf)
	}
	fi, err := entry.Info()
	return fi, nameBuf, err
}
