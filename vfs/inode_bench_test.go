package vfs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/require"
)

func inodeBenchmarkVFS(b *testing.B, union bool) (*VFS, *Dir) {
	b.Helper()
	ctx := context.Background()
	roots := []string{b.TempDir()}
	if union {
		roots = append(roots, b.TempDir())
	}
	for _, root := range roots {
		require.NoError(b, os.Mkdir(filepath.Join(root, "dir"), 0700))
	}
	for i := range 10000 {
		root := roots[0]
		if union && i >= 5000 {
			root = roots[1]
		}
		require.NoError(b, os.WriteFile(filepath.Join(root, "dir", fmt.Sprintf("f%05d", i)), []byte("x"), 0600))
	}
	remote := roots[0]
	if union {
		remote = fmt.Sprintf(":union,upstreams='%s %s':", roots[0], roots[1])
	}
	f, err := fs.NewFs(ctx, remote)
	require.NoError(b, err)
	opt := vfscommon.Opt
	opt.DirCacheTime = fs.Duration(time.Hour)
	opt.PollInterval = 0
	opt.CacheMode = vfscommon.CacheModeOff
	v := New(ctx, f, &opt)
	b.Cleanup(func() {
		require.NoError(b, v.CleanUp())
		v.Shutdown()
	})
	n, err := v.Stat("dir")
	require.NoError(b, err)
	return v, n.(*Dir)
}

func benchmarkDirReadDirRefresh(b *testing.B, union bool) {
	_, dir := inodeBenchmarkVFS(b, union)
	_, err := dir.ReadDirAll()
	require.NoError(b, err)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		b.StopTimer()
		expired := time.Now().Add(-2 * time.Hour)
		dir.walk(func(d *Dir) {
			if !d.read.IsZero() {
				d.read = expired
			}
			for leaf := range d.statRead {
				d.statRead[leaf] = expired
			}
		})
		b.StartTimer()
		_, err := dir.ReadDirAll()
		require.NoError(b, err)
	}
}

func BenchmarkDirReadDirRefresh(b *testing.B) {
	benchmarkDirReadDirRefresh(b, false)
}

func BenchmarkUnionReadDirRefresh(b *testing.B) {
	benchmarkDirReadDirRefresh(b, true)
}

func BenchmarkDirStatRefresh(b *testing.B) {
	v, dir := inodeBenchmarkVFS(b, false)
	_, err := v.Stat("dir/f00000")
	require.NoError(b, err)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		b.StopTimer()
		dir.mu.Lock()
		expired := time.Now().Add(-2 * time.Hour)
		if !dir.read.IsZero() {
			dir.read = expired
		}
		for leaf := range dir.statRead {
			dir.statRead[leaf] = expired
		}
		dir.mu.Unlock()
		b.StartTimer()
		_, err := v.Stat("dir/f00000")
		require.NoError(b, err)
	}
}

func BenchmarkVFSStatCached(b *testing.B) {
	v, _ := inodeBenchmarkVFS(b, false)
	_, err := v.Stat("dir/f00000")
	require.NoError(b, err)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, err := v.Stat("dir/f00000")
		require.NoError(b, err)
	}
}

func BenchmarkDirReadDirCold(b *testing.B) {
	v, _ := inodeBenchmarkVFS(b, false)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		b.StopTimer()
		v.FlushDirCache()
		b.StartTimer()
		n, err := v.Stat("dir")
		require.NoError(b, err)
		_, err = n.(*Dir).ReadDirAll()
		require.NoError(b, err)
	}
}
