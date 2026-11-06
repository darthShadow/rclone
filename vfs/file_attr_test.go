package vfs

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/operations"
	"github.com/rclone/rclone/fstest"
	"github.com/rclone/rclone/vfs/vfscache/writeback"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dHeldWriteBack keeps closed files dirty in the cache until dFlush
const dHeldWriteBack = fs.Duration(time.Hour)

// dWriteFile writes data to a new or truncated file and returns the
// handle, leaving it open
func dWriteFile(t *testing.T, vfs *VFS, path, data string) Handle {
	fh, err := vfs.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
	require.NoError(t, err)
	_, err = fh.WriteString(data)
	require.NoError(t, err)
	return fh
}

// dFlush uploads everything waiting in the write back queue
func dFlush(t *testing.T, vfs *VFS) {
	for _, q := range vfs.cache.Queue()["queue"].([]writeback.QueueInfo) {
		require.NoError(t, vfs.cache.QueueSetExpiry(q.ID, time.Now(), 0))
	}
	vfs.WaitForWriters(waitForWritersDelay)
}

// dStatFile returns the File for path
func dStatFile(t *testing.T, vfs *VFS, path string) *File {
	node, err := vfs.Stat(path)
	require.NoError(t, err)
	return node.(*File)
}

// dExpectedModTime is the modification time the cache item for f reports
func dExpectedModTime(t *testing.T, f *File) time.Time {
	modTime, err := f.d.vfs.cache.Item(f.CachePath()).GetModTime()
	require.NoError(t, err)
	return f._roundModTime(modTime)
}

// dLinkLeaf is a link name longer than the 32 bytes the compiler can
// build a concatenation in on the stack
const dLinkLeaf = "a-link-with-a-name-longer-than-thirty-two-bytes"

// dAttrAllocs returns the allocations per call of Size and ModTime of
// a clean file, or of a clean link if links is set, while another file
// in the same VFS is dirty
func dAttrAllocs(t *testing.T, mode vfscommon.CacheMode, links bool) (size, modTime float64) {
	opt := vfscommon.Opt
	opt.CacheMode = mode
	opt.WriteBack = writeBackDelay
	opt.Links = links
	r, vfs := newTestVFSOpt(t, &opt)

	remote := "dir/file1"
	if links {
		remote = "dir/" + dLinkLeaf + fs.LinkSuffix
	}
	item := r.WriteObject(context.Background(), remote, "file1 contents", t1)
	r.CheckRemoteItems(t, item)
	file := dStatFile(t, vfs, strings.TrimSuffix(remote, fs.LinkSuffix))
	require.Equal(t, links, file.IsSymlink())

	fh := dWriteFile(t, vfs, "dir/dirty", "dirty")
	t.Cleanup(func() { _ = fh.Close() })
	require.Equal(t, int64(len("file1 contents")), file.Size())
	size = testing.AllocsPerRun(1000, func() { file.Size() })
	modTime = testing.AllocsPerRun(1000, func() { file.ModTime() })
	return size, modTime
}

// TestFileAttrCleanFileAllocs checks that Size and ModTime of a clean
// file cost the same with the cache on as with it off, and that a
// clean link costs one more allocation for its cache name.
func TestFileAttrCleanFileAllocs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		links bool
		extra float64
	}{
		{"File", false, 0},
		{"Link", true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var offSize, offModTime float64
			t.Run("Off", func(t *testing.T) {
				offSize, offModTime = dAttrAllocs(t, vfscommon.CacheModeOff, tc.links)
			})
			t.Run("Writes", func(t *testing.T) {
				size, modTime := dAttrAllocs(t, vfscommon.CacheModeWrites, tc.links)
				assert.Equal(t, offSize+tc.extra, size, "Size allocations")
				assert.Equal(t, offModTime+tc.extra, modTime, "ModTime allocations")
			})
		})
	}
}

// BenchmarkFileSizeClean reports the cost of Size for a clean file
// with the write cache on.
func BenchmarkFileSizeClean(b *testing.B) {
	ctx := context.Background()
	root := b.TempDir()
	require.NoError(b, os.WriteFile(root+"/file", []byte("contents"), 0600))
	f, err := fs.NewFs(ctx, root)
	require.NoError(b, err)
	opt := vfscommon.Opt
	opt.CacheMode = vfscommon.CacheModeWrites
	v := New(ctx, f, &opt)
	b.Cleanup(func() {
		require.NoError(b, v.CleanUp())
		v.Shutdown()
	})
	node, err := v.Stat("file")
	require.NoError(b, err)
	file := node.(*File)
	b.ReportAllocs()
	for b.Loop() {
		file.Size()
	}
}

// dDirtyVFS makes a VFS in writes mode whose closed files stay dirty
func dDirtyVFS(t *testing.T) (*VFS, *File) {
	opt := vfscommon.Opt
	opt.CacheMode = vfscommon.CacheModeWrites
	opt.WriteBack = dHeldWriteBack
	r, vfs := newTestVFSOpt(t, &opt)
	file1 := r.WriteObject(context.Background(), "dir/file1", "file1 contents", t1)
	r.CheckRemoteItems(t, file1)
	t.Cleanup(func() { dFlush(t, vfs) })
	return vfs, dStatFile(t, vfs, "dir/file1")
}

// TestFileAttrDirtyItem checks Size and ModTime come from the dirty
// cache item, however the item was dirtied.
func TestFileAttrDirtyItem(t *testing.T) {
	t.Run("WriteThroughOpenHandle", func(t *testing.T) {
		vfs, file := dDirtyVFS(t)
		fh := dWriteFile(t, vfs, "dir/file1", "abc")
		assert.Equal(t, int64(3), file.Size())
		assert.Equal(t, dExpectedModTime(t, file), file.ModTime())
		require.NoError(t, fh.Close())
		assert.Equal(t, int64(3), file.Size())
		assert.Equal(t, dExpectedModTime(t, file), file.ModTime())
	})
	t.Run("OtherFileValue", func(t *testing.T) {
		vfs, file := dDirtyVFS(t)
		other := newFile(file.d, file.dPath, file.o, file.leaf)
		assert.Equal(t, int64(len("file1 contents")), other.Size())
		fh := dWriteFile(t, vfs, "dir/file1", "abcd")
		assert.Equal(t, int64(4), other.Size())
		assert.Equal(t, dExpectedModTime(t, file), other.ModTime())
		require.NoError(t, fh.Close())
		assert.Equal(t, int64(4), other.Size())
	})
	t.Run("FileRename", func(t *testing.T) {
		vfs, _ := dDirtyVFS(t)
		if !operations.CanServerSideMove(vfs.f) {
			t.Skip("skip as can't rename files")
		}
		require.NoError(t, dWriteFile(t, vfs, "dir/file1", "abcde").Close())
		require.NoError(t, vfs.Rename("dir/file1", "dir/file2"))
		renamed := dStatFile(t, vfs, "dir/file2")
		assert.Equal(t, int64(5), renamed.Size())
		assert.Equal(t, dExpectedModTime(t, renamed), renamed.ModTime())
	})
	t.Run("DirRename", func(t *testing.T) {
		vfs, _ := dDirtyVFS(t)
		features := vfs.f.Features()
		if features.DirMove == nil && features.Move == nil && features.Copy == nil {
			t.Skip("skip as can't rename directories")
		}
		require.NoError(t, dWriteFile(t, vfs, "dir/file1", "abcdef").Close())
		require.NoError(t, vfs.Rename("dir", "dir2"))
		renamed := dStatFile(t, vfs, "dir2/file1")
		assert.Equal(t, int64(6), renamed.Size())
		assert.Equal(t, dExpectedModTime(t, renamed), renamed.ModTime())
	})
	t.Run("Reload", func(t *testing.T) {
		opt := vfscommon.Opt
		opt.CacheMode = vfscommon.CacheModeWrites
		opt.WriteBack = dHeldWriteBack
		r := fstest.NewRun(t)
		vfs := New(context.Background(), r.Fremote, &opt)
		require.NoError(t, vfs.Mkdir("dir", 0777))
		require.NoError(t, dWriteFile(t, vfs, "dir/new", "abcdefg").Close())
		require.Equal(t, int64(7), dStatFile(t, vfs, "dir/new").Size())
		vfs.Shutdown()

		// Start again over the same cache directory before the upload.
		vfs2 := New(context.Background(), r.Fremote, &opt)
		t.Cleanup(func() {
			dFlush(t, vfs2)
			cleanupVFS(t, vfs2)
		})
		file := dStatFile(t, vfs2, "dir/new")
		assert.Equal(t, int64(7), file.Size())
		assert.Equal(t, dExpectedModTime(t, file), file.ModTime())
	})
}

// TestFileAttrCleanedItem checks Size stops using the item once it is
// clean or removed.
func TestFileAttrCleanedItem(t *testing.T) {
	t.Run("Uploaded", func(t *testing.T) {
		vfs, file := dDirtyVFS(t)
		require.NoError(t, dWriteFile(t, vfs, "dir/file1", "abc").Close())
		require.Equal(t, int64(3), file.Size())
		dFlush(t, vfs)
		obj, err := vfs.f.NewObject(context.Background(), "dir/file1")
		require.NoError(t, err)
		assert.Equal(t, obj.Size(), file.Size())
		assert.Equal(t, int64(3), file.Size())
	})
	t.Run("CacheRemove", func(t *testing.T) {
		vfs, file := dDirtyVFS(t)
		require.NoError(t, dWriteFile(t, vfs, "dir/file1", "abc").Close())
		require.Equal(t, int64(3), file.Size())
		vfs.cache.Remove(file.CachePath())
		assert.Equal(t, file.o.Size(), file.Size())
	})
}
