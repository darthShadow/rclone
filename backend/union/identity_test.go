//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package union

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/stretchr/testify/require"
)

func TestUnionEntryIdentity(t *testing.T) {
	ctx := context.Background()
	d1, d2 := t.TempDir(), t.TempDir()
	for _, dir := range []string{d1, d2} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "dup"), []byte("duplicate"), 0600))
		require.NoError(t, os.Mkdir(filepath.Join(dir, "dir"), 0700))
	}
	require.NoError(t, os.WriteFile(filepath.Join(d2, "single"), []byte("original"), 0600))
	f, err := fs.NewFs(ctx, fmt.Sprintf(":union,upstreams='%s %s':", d1, d2))
	require.NoError(t, err)
	base, err := local.NewFs(ctx, "local", d2, configmap.Simple{})
	require.NoError(t, err)
	identity := func(t *testing.T, entry fs.DirEntry) uint64 {
		t.Helper()
		id, ok := entry.(fs.EntryIdentifier)
		require.True(t, ok, "%T does not implement fs.EntryIdentifier", entry)
		return id.EntryIdentity()
	}
	baseIdentity := func(t *testing.T) uint64 {
		t.Helper()
		o, err := base.NewObject(ctx, "single")
		require.NoError(t, err)
		id := identity(t, o)
		require.NotZero(t, id)
		return id
	}
	original := baseIdentity(t)
	t.Run("Single", func(t *testing.T) {
		o, err := f.NewObject(ctx, "single")
		require.NoError(t, err)
		require.Equal(t, original, identity(t, o))
	})
	t.Run("ReplaceSingle", func(t *testing.T) {
		require.NoError(t, os.WriteFile(filepath.Join(d2, "replacement"), []byte("replacement"), 0600))
		require.NoError(t, os.Rename(filepath.Join(d2, "replacement"), filepath.Join(d2, "single")))
		o, err := f.NewObject(ctx, "single")
		require.NoError(t, err)
		got := identity(t, o)
		require.NotEqual(t, original, got)
		require.Equal(t, baseIdentity(t), got)
	})
	t.Run("Duplicated", func(t *testing.T) {
		o, err := f.NewObject(ctx, "dup")
		require.NoError(t, err)
		require.Zero(t, identity(t, o))
	})
	t.Run("Directory", func(t *testing.T) {
		entries, err := f.List(ctx, "")
		require.NoError(t, err)
		for _, entry := range entries {
			if entry.Remote() == "dir" {
				_, ok := entry.(fs.EntryIdentifier)
				require.False(t, ok, "%T implements fs.EntryIdentifier", entry)
				return
			}
		}
		t.Fatal("merged directory dir is missing from listing")
	})
}
