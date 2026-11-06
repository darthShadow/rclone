package local

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/text/unicode/norm"
)

// With --local-unicode-normalization the listing reports on-disk names as
// NFC, so a Stat miss on a raw leaf is not proof that the name is absent.
func TestStatUTFNormMiss(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs a filesystem that keeps NFD and NFC names distinct, which only Linux guarantees")
	}
	ctx := context.Background()
	dir := t.TempDir()
	nfd := norm.NFD.String("é.txt")
	nfc := norm.NFC.String("é.txt")
	require.NotEqual(t, nfd, nfc)
	require.NoError(t, os.WriteFile(filepath.Join(dir, nfd), []byte("data"), 0o666))

	for _, utfNorm := range []bool{true, false} {
		t.Run("UTFNorm="+strconv.FormatBool(utfNorm), func(t *testing.T) {
			f, err := NewFs(ctx, "local", dir, configmap.Simple{
				"unicode_normalization": strconv.FormatBool(utfNorm),
			})
			require.NoError(t, err)
			stater, ok := f.(fs.Stater)
			require.True(t, ok, "local must implement fs.Stater")

			entry, err := stater.Stat(ctx, "", nfd)
			require.NoError(t, err)
			assert.NotNil(t, entry)

			entry, err = stater.Stat(ctx, "", nfc)
			assert.Nil(t, entry)
			require.Error(t, err)
			if utfNorm {
				assert.ErrorIs(t, err, fs.ErrorNotImplemented)
				assert.NotErrorIs(t, err, os.ErrNotExist)
			} else {
				assert.ErrorIs(t, err, os.ErrNotExist)
				assert.NotErrorIs(t, err, fs.ErrorNotImplemented)
			}
		})
	}
}

// A name that is on disk in no form is a miss that is not-found without
// normalization and not-implemented with it, on every platform.
func TestStatUTFNormAbsentName(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	for _, utfNorm := range []bool{true, false} {
		t.Run("UTFNorm="+strconv.FormatBool(utfNorm), func(t *testing.T) {
			f, err := NewFs(ctx, "local", dir, configmap.Simple{
				"unicode_normalization": strconv.FormatBool(utfNorm),
			})
			require.NoError(t, err)
			stater, ok := f.(fs.Stater)
			require.True(t, ok, "local must implement fs.Stater")

			entry, err := stater.Stat(ctx, "", "absent.txt")
			assert.Nil(t, entry)
			require.Error(t, err)
			if utfNorm {
				assert.ErrorIs(t, err, fs.ErrorNotImplemented)
				assert.NotErrorIs(t, err, os.ErrNotExist)
			} else {
				assert.ErrorIs(t, err, os.ErrNotExist)
				assert.NotErrorIs(t, err, fs.ErrorNotImplemented)
			}
		})
	}
}

// With links on, the listing reports a symlink "name" as "name.rclonelink",
// so a Stat miss on a name with that suffix is not proof that the name is
// absent. Other misses and every miss without links stay not-found.
func TestStatLinkSuffixMiss(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	for _, links := range []bool{true, false} {
		t.Run("Links="+strconv.FormatBool(links), func(t *testing.T) {
			f, err := NewFs(ctx, "local", dir, configmap.Simple{
				"links": strconv.FormatBool(links),
			})
			require.NoError(t, err)
			stater, ok := f.(fs.Stater)
			require.True(t, ok, "local must implement fs.Stater")

			entry, err := stater.Stat(ctx, "", "absent"+fs.LinkSuffix)
			assert.Nil(t, entry)
			require.Error(t, err)
			if links {
				assert.ErrorIs(t, err, fs.ErrorNotImplemented)
				assert.NotErrorIs(t, err, os.ErrNotExist)
			} else {
				assert.ErrorIs(t, err, os.ErrNotExist)
				assert.NotErrorIs(t, err, fs.ErrorNotImplemented)
			}

			entry, err = stater.Stat(ctx, "", "absent")
			assert.Nil(t, entry)
			require.Error(t, err)
			assert.ErrorIs(t, err, os.ErrNotExist)
			assert.NotErrorIs(t, err, fs.ErrorNotImplemented)
		})
	}
}
