//go:build linux && amd64

package local

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/filter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestFstatatNoFollow_FileInfoCompatibility(t *testing.T) {
	root := t.TempDir()
	filePath := filepath.Join(root, "file.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("hello"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(root, "child"), 0o755))
	require.NoError(t, os.Symlink("file.txt", filepath.Join(root, "link.txt")))

	fd, err := os.Open(root)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, fd.Close())
	}()
	dirfd := int(fd.Fd())
	nameBuf := make([]byte, 0, 16)

	t.Run("regular", func(t *testing.T) {
		fi, nextNameBuf, err := fstatatNoFollow(dirfd, "file.txt", nameBuf)
		require.NoError(t, err)
		nameBuf = nextNameBuf

		want, err := os.Lstat(filePath)
		require.NoError(t, err)

		assert.Equal(t, "file.txt", fi.Name())
		assert.Equal(t, int64(5), fi.Size())
		assert.Equal(t, want.Mode(), fi.Mode())
		assert.False(t, fi.IsDir())
		assert.True(t, fi.ModTime().Equal(want.ModTime()), "expected modtime %v, got %v", want.ModTime(), fi.ModTime())
	})

	t.Run("directory", func(t *testing.T) {
		fi, nextNameBuf, err := fstatatNoFollow(dirfd, "child", nameBuf)
		require.NoError(t, err)
		nameBuf = nextNameBuf

		want, err := os.Lstat(filepath.Join(root, "child"))
		require.NoError(t, err)

		assert.True(t, fi.IsDir())
		assert.Equal(t, want.Mode(), fi.Mode())
		assert.True(t, fi.ModTime().Equal(want.ModTime()), "expected modtime %v, got %v", want.ModTime(), fi.ModTime())
	})

	t.Run("symlink", func(t *testing.T) {
		fi, nextNameBuf, err := fstatatNoFollow(dirfd, "link.txt", nameBuf)
		require.NoError(t, err)
		nameBuf = nextNameBuf

		want, err := os.Lstat(filepath.Join(root, "link.txt"))
		require.NoError(t, err)

		assert.NotZero(t, fi.Mode()&os.ModeSymlink)
		assert.Equal(t, int64(len("file.txt")), fi.Size())
		assert.True(t, fi.ModTime().Equal(want.ModTime()), "expected modtime %v, got %v", want.ModTime(), fi.ModTime())
	})

	t.Run("syscall compat", func(t *testing.T) {
		fi, nextNameBuf, err := fstatatNoFollow(dirfd, "file.txt", nameBuf)
		require.NoError(t, err)
		nameBuf = nextNameBuf

		_, ok := fi.Sys().(*syscall.Stat_t)
		require.True(t, ok, "expected *syscall.Stat_t from Sys(), got %T", fi.Sys())
	})

	t.Run("readTime compat", func(t *testing.T) {
		fi, nextNameBuf, err := fstatatNoFollow(dirfd, "file.txt", nameBuf)
		require.NoError(t, err)
		nameBuf = nextNameBuf

		want, err := os.Lstat(filePath)
		require.NoError(t, err)

		assert.True(t, readTime(cTime, fi).Equal(readTime(cTime, want)), "expected ctime %v, got %v", readTime(cTime, want), readTime(cTime, fi))
	})

	t.Run("readDevice compat", func(t *testing.T) {
		fi, nextNameBuf, err := fstatatNoFollow(dirfd, "file.txt", nameBuf)
		require.NoError(t, err)
		nameBuf = nextNameBuf

		want, err := os.Lstat(filePath)
		require.NoError(t, err)

		assert.Equal(t, readDevice(want, true), readDevice(fi, true))
		assert.Equal(t, uint64(devUnset), readDevice(fi, false))
	})
}

func TestFileModeFromStat(t *testing.T) {
	tests := []struct {
		name string
		in   uint32
		want os.FileMode
	}{
		{name: "regular", in: syscall.S_IFREG | 0o644, want: 0o644},
		{name: "dir", in: syscall.S_IFDIR | 0o755, want: os.ModeDir | 0o755},
		{name: "symlink", in: syscall.S_IFLNK | 0o777, want: os.ModeSymlink | 0o777},
		{name: "fifo", in: syscall.S_IFIFO | 0o644, want: os.ModeNamedPipe | 0o644},
		{name: "socket", in: syscall.S_IFSOCK | 0o755, want: os.ModeSocket | 0o755},
		{name: "char device", in: syscall.S_IFCHR | 0o666, want: os.ModeDevice | os.ModeCharDevice | 0o666},
		{name: "block device", in: syscall.S_IFBLK | 0o660, want: os.ModeDevice | 0o660},
		{name: "setuid", in: syscall.S_IFREG | 0o755 | syscall.S_ISUID, want: os.ModeSetuid | 0o755},
		{name: "setgid", in: syscall.S_IFREG | 0o755 | syscall.S_ISGID, want: os.ModeSetgid | 0o755},
		{name: "sticky dir", in: syscall.S_IFDIR | 0o755 | syscall.S_ISVTX, want: os.ModeDir | os.ModeSticky | 0o755},
		{name: "all specials", in: syscall.S_IFDIR | 0o777 | syscall.S_ISUID | syscall.S_ISGID | syscall.S_ISVTX, want: os.ModeDir | os.ModeSetuid | os.ModeSetgid | os.ModeSticky | 0o777},
		{name: "unknown type", in: uint32(0o150000 | 0o644), want: os.ModeIrregular | 0o644},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, fileModeFromStat(tc.in))
		})
	}
}

func TestFstatatNoFollow_ENOENT(t *testing.T) {
	root := t.TempDir()
	fd, err := os.Open(root)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, fd.Close())
	}()

	fi, _, err := fstatatNoFollow(int(fd.Fd()), "missing.txt", make([]byte, 0, 16))
	assert.Nil(t, fi)
	require.True(t, os.IsNotExist(err), "expected not-exist error, got %v", err)
}

func TestFstatatNoFollow_GrowsReusableNameBuffer(t *testing.T) {
	root := t.TempDir()
	longName := "this-name-exceeds-the-initial-buffer-size.txt"
	require.NoError(t, os.WriteFile(filepath.Join(root, longName), []byte("hello"), 0o600))

	fd, err := os.Open(root)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, fd.Close())
	}()

	nameBuf := make([]byte, 0, 8)
	fi, nextNameBuf, err := fstatatNoFollow(int(fd.Fd()), longName, nameBuf)
	require.NoError(t, err)
	require.NotNil(t, fi)
	assert.Equal(t, longName, fi.Name())
	assert.GreaterOrEqual(t, cap(nextNameBuf), len(longName)+1)
}

func TestFstatatNoFollow_ReusesProvidedNameBuffer(t *testing.T) {
	root := t.TempDir()
	name := "small.txt"
	require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte("hello"), 0o600))

	fd, err := os.Open(root)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, fd.Close())
	}()

	nameBuf := make([]byte, 0, 64)
	initialPtr := unsafe.Pointer(&nameBuf[:cap(nameBuf)][0])

	fi, nextNameBuf, err := fstatatNoFollow(int(fd.Fd()), name, nameBuf)
	require.NoError(t, err)
	require.NotNil(t, fi)
	assert.Equal(t, name, fi.Name())
	assert.Equal(t, initialPtr, unsafe.Pointer(&nextNameBuf[:cap(nextNameBuf)][0]))
}

func TestFstatatNoFollow_NAME_MAXOverflow(t *testing.T) {
	root := t.TempDir()
	fd, err := os.Open(root)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, fd.Close())
	}()

	name := strings.Repeat("a", 256)
	fi, nextNameBuf, err := fstatatNoFollow(int(fd.Fd()), name, nil)
	assert.Nil(t, fi)
	require.True(t, errors.Is(err, syscall.ENAMETOOLONG), "expected ENAMETOOLONG, got %v", err)
	assert.NotZero(t, cap(nextNameBuf))
}

func TestList_FstatatPath_SkipRecentParity(t *testing.T) {
	f, root := newTestLocalFs(t)
	f.opt.SkipRecent = true

	writeTestFile(t, root, "recent.txt")
	oldTime := time.Now().Add(-1 * time.Hour)
	require.NoError(t, os.Chtimes(filepath.Join(root, "recent.txt"), oldTime, oldTime))

	writeTestDir(t, root, "keepdir")
	require.NoError(t, os.Chtimes(root, time.Now(), time.Now()))

	entries, err := f.List(context.Background(), "")
	require.NoError(t, err)
	assert.Equal(t, []string{"keepdir"}, dirEntryRemotes(entries))
}

func TestList_FstatatPath_TranslateSymlinksFilterParity(t *testing.T) {
	f, root := newTestLocalFs(t)
	f.opt.TranslateSymlinks = true

	writeTestFile(t, root, "target.txt")
	require.NoError(t, os.Symlink("target.txt", filepath.Join(root, "alias.txt")))

	ctx, fi := filter.AddConfig(context.Background())
	require.NoError(t, fi.AddRule("+ *"+fs.LinkSuffix))
	require.NoError(t, fi.AddRule("- *"))
	ctx = filter.SetUseFilter(ctx, true)

	entries, err := f.List(ctx, "")
	require.NoError(t, err)
	assert.Equal(t, []string{"alias.txt" + fs.LinkSuffix}, dirEntryRemotes(entries))
}

func TestListStatDirFD_InvalidFDReturnsFalse(t *testing.T) {
	rawfd, ok := listStatDirFD(invalidStatDirFD)
	assert.Equal(t, invalidStatDirFD, rawfd)
	assert.False(t, ok)
}

func TestOpenDirAtReadFD_ClosedFileReturnsErrClosed(t *testing.T) {
	root := t.TempDir()
	fd, err := os.Open(root)
	require.NoError(t, err)
	require.NoError(t, fd.Close())

	statDir, err := openDirAtReadFD(fd)
	require.Equal(t, invalidStatDirFD, statDir)
	require.True(t, errors.Is(err, os.ErrClosed), "expected os.ErrClosed, got %v", err)
}

func TestStatDirEntry_UsesBatchScopedDirFD(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "file.txt"), []byte("hello"), 0o600))

	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.NotEmpty(t, entries)

	var entry os.DirEntry
	for _, candidate := range entries {
		if candidate.Name() == "file.txt" {
			entry = candidate
			break
		}
	}
	require.NotNil(t, entry)

	statDir, err := os.Open(root)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, statDir.Close())
	}()

	statDirFD, ok := listStatDirFD(int(statDir.Fd()))
	require.True(t, ok)

	cachedEntry := cachedDirEntry{
		DirEntry:  entry,
		statDirFD: statDirFD,
		useStatFD: true,
	}
	fi, _, err := statDirEntry(&cachedEntry, make([]byte, 0, 16))
	require.NoError(t, err)

	want, err := os.Lstat(filepath.Join(root, "file.txt"))
	require.NoError(t, err)

	assert.Equal(t, want.Name(), fi.Name())
	assert.Equal(t, want.Size(), fi.Size())
	assert.Equal(t, want.Mode(), fi.Mode())
	assert.True(t, fi.ModTime().Equal(want.ModTime()), "expected modtime %v, got %v", want.ModTime(), fi.ModTime())
}

func TestOpenDirAtReadFD_ReturnsRawFD(t *testing.T) {
	root := t.TempDir()
	fd, err := os.Open(root)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, fd.Close())
	}()

	statDirFD, err := openDirAtReadFD(fd)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, unix.Close(statDirFD))
	}()

	var st unix.Stat_t
	err = unix.Fstat(statDirFD, &st)
	require.NoError(t, err)
}
