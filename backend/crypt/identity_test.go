//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package crypt_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/crypt"
	"github.com/rclone/rclone/backend/local"
	_ "github.com/rclone/rclone/backend/memory"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/obscure"
	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/fs/operations"
	"github.com/stretchr/testify/require"
)

func TestCryptEntryIdentity(t *testing.T) {
	ctx := context.Background()
	newCrypt := func(t *testing.T, remote string) (*crypt.Fs, *crypt.Cipher) {
		t.Helper()
		config := configmap.Simple{
			"remote":                    remote,
			"password":                  obscure.MustObscure("potato"),
			"filename_encryption":       "standard",
			"filename_encoding":         "base32",
			"directory_name_encryption": "true",
			"suffix":                    ".bin",
		}
		f, err := crypt.NewFs(ctx, "crypt", "", config)
		require.NoError(t, err)
		cipher, err := crypt.NewCipher(config)
		require.NoError(t, err)
		return f.(*crypt.Fs), cipher
	}
	put := func(t *testing.T, f *crypt.Fs, name, content string) {
		t.Helper()
		info := object.NewStaticObjectInfo(name, time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC), int64(len(content)), true, nil, nil)
		_, err := f.Put(ctx, strings.NewReader(content), info)
		require.NoError(t, err)
	}
	identity := func(t *testing.T, entry fs.DirEntry) uint64 {
		t.Helper()
		id, ok := entry.(fs.EntryIdentifier)
		require.True(t, ok, "%T does not implement fs.EntryIdentifier", entry)
		return id.EntryIdentity()
	}
	dir := t.TempDir()
	base, err := local.NewFs(ctx, "local", dir, configmap.Simple{})
	require.NoError(t, err)
	f, cipher := newCrypt(t, dir)
	put(t, f, "file", "original")
	put(t, f, "other", "replacement contents")
	_, err = f.MkdirMetadata(ctx, "dir", nil)
	require.NoError(t, err)
	baseIdentity := func(t *testing.T, name string, directory bool) uint64 {
		t.Helper()
		var entry fs.DirEntry
		var err error
		if directory {
			entry, err = base.(fs.Stater).Stat(ctx, "", cipher.EncryptDirName(name))
		} else {
			entry, err = base.NewObject(ctx, cipher.EncryptFileName(name))
		}
		require.NoError(t, err)
		id := identity(t, entry)
		require.NotZero(t, id)
		return id
	}
	original := baseIdentity(t, "file", false)
	for _, tc := range []struct {
		name      string
		remote    string
		directory bool
	}{
		{"ListedFile", "file", false},
		{"ListedDir", "dir", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries, err := f.List(ctx, "")
			require.NoError(t, err)
			for _, entry := range entries {
				if entry.Remote() == tc.remote {
					require.Equal(t, baseIdentity(t, tc.remote, tc.directory), identity(t, entry))
					return
				}
			}
			t.Fatalf("entry %q is missing from listing", tc.remote)
		})
	}
	t.Run("NewObject", func(t *testing.T) {
		o, err := f.NewObject(ctx, "file")
		require.NoError(t, err)
		require.Equal(t, original, identity(t, o))
	})
	t.Run("MkdirMetadata", func(t *testing.T) {
		entry, err := f.MkdirMetadata(ctx, "made", nil)
		require.NoError(t, err)
		require.Equal(t, baseIdentity(t, "made", true), identity(t, entry))
	})
	t.Run("ReplaceByRename", func(t *testing.T) {
		dst, err := f.NewObject(ctx, "file")
		require.NoError(t, err)
		src, err := f.NewObject(ctx, "other")
		require.NoError(t, err)
		_, err = operations.Move(ctx, f, dst, "file", src)
		require.NoError(t, err)
		o, err := f.NewObject(ctx, "file")
		require.NoError(t, err)
		got := identity(t, o)
		require.NotEqual(t, original, got)
		require.Equal(t, baseIdentity(t, "file", false), got)
	})
	t.Run("OverMemory", func(t *testing.T) {
		f, _ := newCrypt(t, ":memory:"+t.Name())
		put(t, f, "file", "contents")
		require.NoError(t, f.Mkdir(ctx, "dir"))
		put(t, f, "dir/child", "directory contents")
		entries, err := f.List(ctx, "")
		require.NoError(t, err)
		require.Len(t, entries, 2)
		seen := map[string]bool{}
		for _, entry := range entries {
			seen[entry.Remote()] = true
			require.Zero(t, identity(t, entry))
		}
		require.Equal(t, map[string]bool{"file": true, "dir": true}, seen)
	})
}
