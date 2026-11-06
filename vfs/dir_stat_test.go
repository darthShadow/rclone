package vfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/filter"
	"github.com/rclone/rclone/fstest"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cListFs counts List calls and fails them with listErr when it is set.
// It has no Stat, so it models a backend without fs.Stater.
type cListFs struct {
	fs.Fs
	lists   atomic.Int32
	listErr error
}

func (f *cListFs) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	f.lists.Add(1)
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.Fs.List(ctx, dir)
}

// cStatFs adds a Stat to cListFs. Stat returns the error missFor gives
// for the leaf, or forwards to the wrapped Fs when missFor is nil or
// returns nil. It returns fs.ErrorNotImplemented if the wrapped Fs has
// no fs.Stater.
type cStatFs struct {
	*cListFs
	missFor func(leaf string) error
	stats   atomic.Int32
}

func (f *cStatFs) Stat(ctx context.Context, dir, leaf string) (fs.DirEntry, error) {
	f.stats.Add(1)
	if f.missFor != nil {
		if err := f.missFor(leaf); err != nil {
			return nil, err
		}
	}
	stater, ok := f.cListFs.Fs.(fs.Stater)
	if !ok {
		return nil, fs.ErrorNotImplemented
	}
	return stater.Stat(ctx, dir, leaf)
}

// cAlways makes Stat return err for every leaf.
func cAlways(err error) func(string) error {
	return func(string) error { return err }
}

// cNewStatFs wraps f in a cStatFs that returns err from Stat for every
// leaf. A nil err forwards Stat to f.
func cNewStatFs(f fs.Fs, err error) *cStatFs {
	w := &cStatFs{cListFs: &cListFs{Fs: f}}
	if err != nil {
		w.missFor = cAlways(err)
	}
	return w
}

// cNewRoot makes a VFS over f with Unicode normalization, case-insensitive
// matching and links off, whatever the platform defaults are, and returns
// its root directory. modify may change the config and options
// before the VFS is created.
func cNewRoot(t *testing.T, f fs.Fs, modify func(ctx context.Context, ci *fs.ConfigInfo, opt *vfscommon.Options) context.Context) *Dir {
	ctx, ci := fs.AddConfig(context.Background())
	ci.NoUnicodeNormalization = true
	opt := vfscommon.Opt
	opt.CaseInsensitive = false
	opt.Links = false
	opt.DirCacheTime = fs.Duration(time.Hour)
	opt.PollInterval = 0
	opt.CacheMode = vfscommon.CacheModeOff
	if modify != nil {
		ctx = modify(ctx, ci, &opt)
	}
	v := New(ctx, f, &opt)
	t.Cleanup(func() { cleanupVFS(t, v) })
	root, err := v.Root()
	require.NoError(t, err)
	return root
}

// cMakeStale marks the directory as never read.
func cMakeStale(d *Dir) {
	d.mu.Lock()
	d.read = time.Time{}
	d.mu.Unlock()
}

// cRequireStater skips the test if the backend has no fs.Stater.
func cRequireStater(t *testing.T, f fs.Fs) {
	if _, ok := f.(fs.Stater); !ok {
		t.Skip("backend does not implement fs.Stater")
	}
}

// cDefiniteMisses are Stat errors that prove the leaf is absent.
func cDefiniteMisses() map[string]error {
	return map[string]error{
		"ObjectNotFound": fs.ErrorObjectNotFound,
		"DirNotFound":    fs.ErrorDirNotFound,
		"ENOENT":         ENOENT,
		"ErrNotExist":    os.ErrNotExist,
		"Errno":          syscall.ENOENT,
		"PathError":      &os.PathError{Op: "lstat", Path: "x", Err: syscall.ENOENT},
		"Wrapped":        fmt.Errorf("upstream: %w", fs.ErrorObjectNotFound),
		"Joined":         errors.Join(fs.ErrorObjectNotFound, ENOENT),
		"NestedJoin": errors.Join(
			fmt.Errorf("u1: %w", fs.ErrorObjectNotFound),
			errors.Join(fs.ErrorDirNotFound, fmt.Errorf("u2: %w", syscall.ENOENT)),
		),
	}
}

// A definite not-found with normalization and links off returns ENOENT
// without listing the directory.
func TestCDirStatDefiniteMissSkipsListing(t *testing.T) {
	for name, statErr := range cDefiniteMisses() {
		t.Run(name, func(t *testing.T) {
			r := fstest.NewRun(t)
			cRequireStater(t, r.Fremote)
			w := cNewStatFs(r.Fremote, statErr)
			root := cNewRoot(t, w, nil)

			node, err := root.stat("missing")
			assert.Nil(t, node)
			assert.Equal(t, ENOENT, err)
			assert.Equal(t, int32(1), w.stats.Load(), "Stat calls")
			assert.Equal(t, int32(0), w.lists.Load(), "List calls")
		})
	}
}

// The backend's own Stat reports a miss that skips the listing.
func TestCDirStatBackendMissSkipsListing(t *testing.T) {
	r := fstest.NewRun(t)
	cRequireStater(t, r.Fremote)
	w := cNewStatFs(r.Fremote, nil)
	root := cNewRoot(t, w, nil)

	node, err := root.stat("missing")
	assert.Nil(t, node)
	assert.Equal(t, ENOENT, err)
	assert.Equal(t, int32(1), w.stats.Load(), "Stat calls")
	assert.Equal(t, int32(0), w.lists.Load(), "List calls")
}

// Unicode normalization, case-insensitive matching and links each
// keep the listing fallback after a definite not-found.
func TestCDirStatNormalizationKeepsListing(t *testing.T) {
	for _, test := range []struct {
		name   string
		modify func(ctx context.Context, ci *fs.ConfigInfo, opt *vfscommon.Options) context.Context
	}{
		{"UnicodeNormalization", func(ctx context.Context, ci *fs.ConfigInfo, opt *vfscommon.Options) context.Context {
			ci.NoUnicodeNormalization = false
			return ctx
		}},
		{"CaseInsensitive", func(ctx context.Context, ci *fs.ConfigInfo, opt *vfscommon.Options) context.Context {
			opt.CaseInsensitive = true
			return ctx
		}},
		{"IgnoreCaseSync", func(ctx context.Context, ci *fs.ConfigInfo, opt *vfscommon.Options) context.Context {
			ci.IgnoreCaseSync = true
			return ctx
		}},
		{"Links", func(ctx context.Context, ci *fs.ConfigInfo, opt *vfscommon.Options) context.Context {
			opt.Links = true
			return ctx
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := fstest.NewRun(t)
			cRequireStater(t, r.Fremote)
			w := cNewStatFs(r.Fremote, fs.ErrorObjectNotFound)
			root := cNewRoot(t, w, test.modify)

			node, err := root.stat("missing")
			assert.Nil(t, node)
			assert.Equal(t, ENOENT, err)
			assert.Equal(t, int32(1), w.lists.Load(), "List calls")
		})
	}
}

// Anything other than a definite not-found keeps the listing fallback.
func TestCDirStatOtherErrorsKeepListing(t *testing.T) {
	other := errors.New("backend broke")
	for name, statErr := range map[string]error{
		"NotImplemented":       fs.ErrorNotImplemented,
		"JoinedNotImplemented": errors.Join(fs.ErrorObjectNotFound, fs.ErrorNotImplemented),
		"JoinedOther":          errors.Join(fs.ErrorObjectNotFound, other),
		"NestedOther": errors.Join(
			fs.ErrorObjectNotFound,
			errors.Join(ENOENT, fmt.Errorf("u2: %w", other)),
		),
		"Other":    other,
		"NotADir":  &os.PathError{Op: "lstat", Path: "x", Err: syscall.ENOTDIR},
		"Canceled": context.Canceled,
	} {
		t.Run(name, func(t *testing.T) {
			r := fstest.NewRun(t)
			w := cNewStatFs(r.Fremote, statErr)
			root := cNewRoot(t, w, nil)

			node, err := root.stat("missing")
			assert.Nil(t, node)
			assert.Equal(t, ENOENT, err)
			assert.Equal(t, int32(1), w.lists.Load(), "List calls")
		})
	}
}

// A backend without fs.Stater keeps the listing fallback.
func TestCDirStatNoStaterKeepsListing(t *testing.T) {
	r := fstest.NewRun(t)
	w := &cListFs{Fs: r.Fremote}
	root := cNewRoot(t, w, nil)

	node, err := root.stat("missing")
	assert.Nil(t, node)
	assert.Equal(t, ENOENT, err)
	assert.Equal(t, int32(1), w.lists.Load(), "List calls")
}

// An entry that Stat finds but the filter excludes keeps the listing
// fallback.
func TestCDirStatFilteredEntryKeepsListing(t *testing.T) {
	r := fstest.NewRun(t)
	cRequireStater(t, r.Fremote)
	r.WriteObject(context.Background(), "excluded.txt", "data", t1)
	w := cNewStatFs(r.Fremote, nil)
	root := cNewRoot(t, w, func(ctx context.Context, ci *fs.ConfigInfo, opt *vfscommon.Options) context.Context {
		fi, err := filter.NewFilter(nil)
		require.NoError(t, err)
		require.NoError(t, fi.Add(false, "excluded.txt"))
		require.NoError(t, fi.Add(true, "*"))
		return filter.ReplaceConfig(ctx, fi)
	})

	node, err := root.stat("excluded.txt")
	assert.Nil(t, node)
	assert.Equal(t, ENOENT, err)
	assert.Equal(t, int32(1), w.lists.Load(), "List calls")
}

// A multi-error holding a not-found and a not-implemented does not
// stop the listing from finding a leaf the backend has.
func TestCDirStatMixedErrorStillFindsListedNode(t *testing.T) {
	r := fstest.NewRun(t)
	r.WriteObject(context.Background(), "present", "data", t1)
	w := cNewStatFs(r.Fremote, errors.Join(fs.ErrorObjectNotFound, fs.ErrorNotImplemented))
	root := cNewRoot(t, w, nil)

	node, err := root.stat("present")
	require.NoError(t, err)
	assert.Equal(t, "present", node.Name())
	assert.Equal(t, int32(1), w.lists.Load(), "List calls")
}

// With links on, the listing renames name.rclonelink to name, so a
// not-found for name must not skip it.
func TestCDirStatLinksFindsSymlink(t *testing.T) {
	r := fstest.NewRun(t)
	r.WriteObject(context.Background(), "name"+fs.LinkSuffix, "target", t1)
	w := cNewStatFs(r.Fremote, fs.ErrorObjectNotFound)
	root := cNewRoot(t, w, func(ctx context.Context, ci *fs.ConfigInfo, opt *vfscommon.Options) context.Context {
		opt.Links = true
		return ctx
	})

	node, err := root.stat("name")
	require.NoError(t, err)
	assert.Equal(t, "name", node.Name())
	assert.Equal(t, int32(1), w.lists.Load(), "List calls")
}

// A cached node whose object was deleted externally is dropped by the
// listing, not kept by the skip.
func TestCDirStatCachedNodeDeletedExternally(t *testing.T) {
	r := fstest.NewRun(t)
	ctx := context.Background()
	r.WriteObject(ctx, "gone", "data", t1)
	w := cNewStatFs(r.Fremote, fs.ErrorObjectNotFound)
	root := cNewRoot(t, w, nil)

	_, err := root.ReadDirAll()
	require.NoError(t, err)
	o, err := r.Fremote.NewObject(ctx, "gone")
	require.NoError(t, err)
	require.NoError(t, o.Remove(ctx))
	cMakeStale(root)
	listsBefore := w.lists.Load()

	node, err := root.stat("gone")
	assert.Nil(t, node)
	assert.Equal(t, ENOENT, err)
	assert.Equal(t, listsBefore+1, w.lists.Load(), "the listing must run for a cached leaf")
}

// A proven miss returns ENOENT even if the listing would have failed.
func TestCDirStatMissIgnoresListError(t *testing.T) {
	r := fstest.NewRun(t)
	w := cNewStatFs(r.Fremote, fs.ErrorObjectNotFound)
	w.listErr = errors.New("list failed")
	root := cNewRoot(t, w, nil)

	node, err := root.stat("missing")
	assert.Nil(t, node)
	assert.Equal(t, ENOENT, err)
	assert.Equal(t, int32(0), w.lists.Load(), "List calls")
}

// Misses in a stale directory each cost one Stat and no listing, so
// the directory stays stale.
func TestCDirStatRepeatedMissesStatEachName(t *testing.T) {
	const n = 5
	r := fstest.NewRun(t)
	w := cNewStatFs(r.Fremote, fs.ErrorObjectNotFound)
	root := cNewRoot(t, w, nil)

	for i := range n {
		_, err := root.stat(fmt.Sprintf("missing%d", i))
		assert.Equal(t, ENOENT, err)
	}
	assert.Equal(t, int32(n), w.stats.Load(), "Stat calls")
	assert.Equal(t, int32(0), w.lists.Load(), "List calls")
	root.mu.RLock()
	_, stale := root._age(time.Now())
	root.mu.RUnlock()
	assert.True(t, stale, "the directory stays stale")
}

// The metadata-file lookup still runs for a leaf the skip proved missing.
func TestCDirStatMissStillFindsMetadataFile(t *testing.T) {
	r := fstest.NewRun(t)
	cRequireStater(t, r.Fremote)
	r.WriteObject(context.Background(), "file", "data", t1)
	w := cNewStatFs(r.Fremote, nil)
	w.missFor = func(leaf string) error {
		if leaf == "file.metadata" {
			return fs.ErrorObjectNotFound
		}
		return nil
	}
	root := cNewRoot(t, w, func(ctx context.Context, ci *fs.ConfigInfo, opt *vfscommon.Options) context.Context {
		opt.MetadataExtension = ".metadata"
		return ctx
	})

	node, err := root.stat("file.metadata")
	require.NoError(t, err)
	assert.Equal(t, "file.metadata", node.Name())
	assert.Equal(t, int32(0), w.lists.Load(), "List calls")
}

// A local remote with links = true lists a symlink "name" as
// "name.rclonelink" while vfs.Opt.Links is off, so its Stat miss for the
// suffixed name must not skip the listing.
func TestCDirStatLocalLinksFindsTranslatedName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs symlinks")
	}
	ctx := context.Background()
	dir := t.TempDir()
	require.NoError(t, os.Symlink("target", filepath.Join(dir, "name")))
	f, err := local.NewFs(ctx, "local", dir, configmap.Simple{
		"links":                 "true",
		"unicode_normalization": "false",
	})
	require.NoError(t, err)
	w := cNewStatFs(f, nil)
	root := cNewRoot(t, w, nil)

	node, err := root.stat("name" + fs.LinkSuffix)
	require.NoError(t, err)
	assert.Equal(t, "name"+fs.LinkSuffix, node.Name())
	assert.Equal(t, int32(1), w.lists.Load(), "List calls")
}
