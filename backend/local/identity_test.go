//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package local

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/rclone/rclone/fs"
	"github.com/stretchr/testify/require"
)

func TestEntryIdentity(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	f, err := fs.NewFs(ctx, dir)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "file"), []byte("original"), 0600))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0700))
	expected := func(remote string) uint64 {
		t.Helper()
		info, err := os.Lstat(filepath.Join(dir, remote))
		require.NoError(t, err)
		st := info.Sys().(*syscall.Stat_t)
		return localIdentity(uint64(st.Dev), uint64(st.Ino)) // nolint: unconvert
	}
	identity := func(entry fs.DirEntry) uint64 {
		t.Helper()
		e, ok := entry.(fs.EntryIdentifier)
		require.True(t, ok, "%T must implement fs.EntryIdentifier", entry)
		return e.EntryIdentity()
	}
	t.Run("Vectors", func(t *testing.T) {
		for _, test := range []struct{ dev, ino, want uint64 }{
			{44, 1<<40 + 5, 3569360960551211929},
			{45, 1<<40 + 5, 14970075779874410420},
			{44, 0, 0},
			{1, 0x9e3779b97f4a7c15, 1},
		} {
			require.Equal(t, test.want, localIdentity(test.dev, test.ino))
		}
	})
	for _, test := range []struct{ name, remote string }{{"ListedFile", "file"}, {"ListedDir", "sub"}} {
		t.Run(test.name, func(t *testing.T) {
			entries, err := f.List(ctx, "")
			require.NoError(t, err)
			for _, entry := range entries {
				if entry.Remote() == test.remote {
					require.Equal(t, expected(test.remote), identity(entry))
					return
				}
			}
			t.Fatalf("missing entry %q", test.remote)
		})
	}
	t.Run("Stat", func(t *testing.T) {
		entry, err := f.(fs.Stater).Stat(ctx, "", "file")
		require.NoError(t, err)
		require.Equal(t, expected("file"), identity(entry))
	})
	t.Run("NewObject", func(t *testing.T) {
		obj, err := f.NewObject(ctx, "file")
		require.NoError(t, err)
		require.Equal(t, expected("file"), identity(obj))
	})
	t.Run("MkdirMetadata", func(t *testing.T) {
		entry, err := f.Features().MkdirMetadata(ctx, "made", nil)
		require.NoError(t, err)
		require.Equal(t, expected("made"), identity(entry))
	})
	t.Run("RenameOver", func(t *testing.T) {
		before := expected("file")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "other"), []byte("replacement"), 0600))
		require.NoError(t, os.Rename(filepath.Join(dir, "other"), filepath.Join(dir, "file")))
		obj, err := f.NewObject(ctx, "file")
		require.NoError(t, err)
		require.NotEqual(t, before, identity(obj))
		require.Equal(t, expected("file"), identity(obj))
	})
	t.Run("InPlace", func(t *testing.T) {
		before := expected("file")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "file"), []byte("updated contents"), 0600))
		obj, err := f.NewObject(ctx, "file")
		require.NoError(t, err)
		require.Equal(t, before, identity(obj))
	})
}
