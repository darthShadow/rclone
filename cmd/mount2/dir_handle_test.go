//go:build linux

package mount2

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	stdlog "log"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	fusefs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/rclone/rclone/cmd/mountlib"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/log"
	"github.com/rclone/rclone/fs/rc"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// dirCacheFlags are the flags OpendirHandle returns for a handle that uses
// the kernel directory cache.
const dirCacheFlags = fuse.FOPEN_CACHE_DIR | fuse.FOPEN_KEEP_CACHE

// newDirCacheFixture returns a node fixture with the kernel directory cache on.
func newDirCacheFixture(t *testing.T, names ...string) *mount2NodeFixture {
	t.Helper()
	fixture := newMount2NodeFixture(t, names...)
	fixture.fsys.dirCache = true
	return fixture
}

// openDirHandle opens n and checks the flags OpendirHandle returns before
// the caller looks at any state.
func openDirHandle(t *testing.T, fixture *mount2NodeFixture, n *Node, wantFlags uint32) *mount2DirHandle {
	t.Helper()
	fh, flags, errno := n.OpendirHandle(fixture.ctx, 0)
	require.Equal(t, syscall.Errno(0), errno, "OpendirHandle errno")
	require.Equal(t, wantFlags, flags, "OpendirHandle flags")
	handle, ok := fh.(*mount2DirHandle)
	require.True(t, ok, "handle is %T", fh)
	return handle
}

// readDirHandle reads h to the end of the directory.
func readDirHandle(t *testing.T, fixture *mount2NodeFixture, h *mount2DirHandle) []fuse.DirEntry {
	t.Helper()
	var entries []fuse.DirEntry
	for {
		entry, errno := h.Readdirent(fixture.ctx)
		require.Equal(t, syscall.Errno(0), errno, "Readdirent errno")
		if entry == nil {
			return entries
		}
		entries = append(entries, *entry)
	}
}

func entryNames(entries []fuse.DirEntry) []string {
	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = entry.Name
	}
	return names
}

// sameEntries reports whether a and b share their backing array.
func sameEntries(a, b []fuse.DirEntry) bool {
	return len(a) > 0 && len(b) > 0 && &a[0] == &b[0]
}

// dirStateOf returns a copy of the cache state of n.
func dirStateOf(t *testing.T, n *Node) (key dirKey, snap []fuse.DirEntry, caching int) {
	t.Helper()
	require.NotNil(t, n.dir, "node has no directory cache state")
	n.dir.mu.Lock()
	defer n.dir.mu.Unlock()
	return n.dir.key, n.dir.snap, n.dir.caching
}

func TestDirCacheSharedEntries(t *testing.T) {
	fixture := newDirCacheFixture(t, "a", "b", "c")

	h1 := openDirHandle(t, fixture, fixture.root, dirCacheFlags)
	h2 := openDirHandle(t, fixture, fixture.root, dirCacheFlags)
	assert.True(t, h1.caching)
	assert.True(t, h2.caching)
	require.True(t, sameEntries(h1.entries, h2.entries), "caching handles should share one snapshot")
	assert.Zero(t, fixture.callbacks.inodeCallCount())

	_, snap, caching := dirStateOf(t, fixture.root)
	assert.True(t, sameEntries(snap, h1.entries))
	assert.Equal(t, 2, caching)

	before := slices.Clone(snap)
	got1 := readDirHandle(t, fixture, h1)
	got2 := readDirHandle(t, fixture, h2)
	assert.Equal(t, got1, got2)
	assert.ElementsMatch(t, []string{".", "..", "a", "b", "c"}, entryNames(got1))
	for i, entry := range got1 {
		assert.Equal(t, uint64(i+1), entry.Off, "entry %q", entry.Name)
	}
	assert.Equal(t, before, snap, "reads must not change the shared entries")
}

func TestDirCacheRowC(t *testing.T) {
	fixture := newDirCacheFixture(t, "a", "b")

	h1 := openDirHandle(t, fixture, fixture.root, dirCacheFlags)
	key1, snap1, _ := dirStateOf(t, fixture.root)

	// A change while a caching handle is open: keep the cache, bypass it.
	fixture.dir.AddVirtual("new", 1, false)
	h2 := openDirHandle(t, fixture, fixture.root, fuse.FOPEN_KEEP_CACHE)
	assert.False(t, h2.caching)
	assert.Contains(t, entryNames(h2.entries), "new")
	key2, snap2, caching := dirStateOf(t, fixture.root)
	assert.Equal(t, key1, key2)
	assert.True(t, sameEntries(snap1, snap2))
	assert.Equal(t, 1, caching)
	assert.Zero(t, fixture.callbacks.inodeCallCount())

	// After the caching handle is released the next open moves to the new listing.
	h1.Releasedir(fixture.ctx, 0)
	h2.Releasedir(fixture.ctx, 0)
	h3 := openDirHandle(t, fixture, fixture.root, dirCacheFlags)
	assert.Equal(t, []inodeNotifyCall{{node: fuse.FUSE_ROOT_ID}}, fixture.callbacks.recordedInodeCalls())
	key3, snap3, _ := dirStateOf(t, fixture.root)
	assert.NotEqual(t, key1.generation, key3.generation)
	assert.False(t, sameEntries(snap1, snap3))
	assert.Contains(t, entryNames(snap3), "new")
	assert.True(t, sameEntries(snap3, h3.entries))
}

func TestDirCacheCaptureAtOpen(t *testing.T) {
	fixture := newDirCacheFixture(t, "a", "b")

	h := openDirHandle(t, fixture, fixture.root, dirCacheFlags)
	fixture.dir.AddVirtual("new", 1, false)

	require.Equal(t, syscall.Errno(0), h.Seekdir(fixture.ctx, 2))
	assert.ElementsMatch(t, []string{"a", "b"}, entryNames(readDirHandle(t, fixture, h)))

	require.Equal(t, syscall.Errno(0), h.Seekdir(fixture.ctx, 0))
	assert.ElementsMatch(t, []string{".", "..", "a", "b"}, entryNames(readDirHandle(t, fixture, h)))
}

func TestDirCacheKeepUntilChange(t *testing.T) {
	fixture := newDirCacheFixture(t, "a", "b")

	h1 := openDirHandle(t, fixture, fixture.root, dirCacheFlags)
	_, snap1, _ := dirStateOf(t, fixture.root)
	h1.Releasedir(fixture.ctx, 0)

	// The snapshot outlives the last handle.
	_, snap, caching := dirStateOf(t, fixture.root)
	assert.Zero(t, caching)
	require.True(t, sameEntries(snap1, snap), "snapshot must be kept after the last close")

	// Same listing: the same snapshot, no new capture, no notify.
	h2 := openDirHandle(t, fixture, fixture.root, dirCacheFlags)
	assert.True(t, sameEntries(snap1, h2.entries))
	assert.Zero(t, fixture.callbacks.inodeCallCount())
	h2.Releasedir(fixture.ctx, 0)

	// A change: one notify, both flags, a new snapshot listing the change.
	fixture.dir.AddVirtual("new", 1, false)
	h3 := openDirHandle(t, fixture, fixture.root, dirCacheFlags)
	assert.Equal(t, 1, fixture.callbacks.inodeCallCount())
	assert.False(t, sameEntries(snap1, h3.entries))
	assert.Contains(t, entryNames(h3.entries), "new")
}

func TestDirCacheParentMove(t *testing.T) {
	fixture := newDirCacheFixture(t)
	require.NoError(t, fixture.vfs.AddVirtual("sub", 0, true))
	subNode, err := fixture.dir.Stat("sub")
	require.NoError(t, err)
	subDir, ok := subNode.(*vfs.Dir)
	require.True(t, ok, "node is %T", subNode)
	sub := attachMount2Node(fixture.ctx, t, fixture.fsys, fixture.root, subDir)

	// A second parent that resolves "sub" to the same vfs.Dir, so only the
	// parent inode changes.
	const otherIno = 424242
	other := &Node{fsys: fixture.fsys}
	other.vfsDir.Store(fixture.dir)
	otherInode := fixture.root.NewInode(fixture.ctx, other, fusefs.StableAttr{Mode: fuse.S_IFDIR, Ino: otherIno})
	require.True(t, fixture.root.EmbeddedInode().AddChild("other", otherInode, false))

	h1 := openDirHandle(t, fixture, sub, dirCacheFlags)
	key1, snap1, _ := dirStateOf(t, sub)
	assert.Equal(t, fixture.dir.Inode(), key1.parentIno)
	h1.Releasedir(fixture.ctx, 0)

	success, _ := fixture.root.EmbeddedInode().RmChild("sub")
	require.True(t, success)
	require.True(t, otherInode.AddChild("sub", sub.EmbeddedInode(), false))

	h2 := openDirHandle(t, fixture, sub, dirCacheFlags)
	assert.Equal(t, 1, fixture.callbacks.inodeCallCount())
	key2, snap2, _ := dirStateOf(t, sub)
	assert.Equal(t, key1.generation, key2.generation, "only the parent changed")
	assert.Equal(t, uint64(otherIno), key2.parentIno)
	assert.False(t, sameEntries(snap1, snap2))
	assert.Equal(t, "..", h2.entries[1].Name)
	assert.Equal(t, uint64(otherIno), h2.entries[1].Ino)
}

func TestDirCacheDisabled(t *testing.T) {
	fixture := newMount2NodeFixture(t, "a", "b")
	require.False(t, fixture.fsys.dirCache)

	want, err := vfs.MapReadDir(fixture.dir, vfsNodeToDirEntry, 0)
	require.NoError(t, err)

	for range 2 {
		h := openDirHandle(t, fixture, fixture.root, 0)
		assert.False(t, h.caching)
		assert.ElementsMatch(t, append([]string{".", ".."}, entryNames(want)...), entryNames(h.entries))
		fixture.dir.AddVirtual("new", 1, false)
		want, err = vfs.MapReadDir(fixture.dir, vfsNodeToDirEntry, 0)
		require.NoError(t, err)
		h.Releasedir(fixture.ctx, 0)
	}
	assert.Zero(t, fixture.callbacks.inodeCallCount())
}

func TestDirCacheStrandedFillLog(t *testing.T) {
	const want = `kernel directory cache for "/" is not completing; reads stay uncached until the directory is evicted`

	var (
		mu   sync.Mutex
		logs []string
	)
	ci := fs.GetConfig(context.Background())
	oldConfigLevel := ci.LogLevel
	ci.LogLevel = fs.LogLevelInfo
	oldLevel := log.Handler.SetLevel(slog.LevelInfo)
	log.Handler.SetOutput(func(_ slog.Level, text string) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, text)
	})
	t.Cleanup(func() {
		log.Handler.ResetOutput()
		log.Handler.SetLevel(oldLevel)
		ci.LogLevel = oldConfigLevel
	})
	logged := func() int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, text := range logs {
			if strings.Contains(text, want) {
				n++
			}
		}
		return n
	}

	fixture := newDirCacheFixture(t, "a", "b", "c", "d", "e")

	// One caching handle reads from offset 0 to the end of the directory.
	h := openDirHandle(t, fixture, fixture.root, dirCacheFlags)
	readDirHandle(t, fixture, h)
	h.Releasedir(fixture.ctx, 0)
	fixture.root.dir.mu.Lock()
	assert.True(t, fixture.root.dir.runDone)
	fixture.root.dir.mu.Unlock()

	// A run that starts at offset 5 does not count.
	h = openDirHandle(t, fixture, fixture.root, dirCacheFlags)
	require.Equal(t, syscall.Errno(0), h.Seekdir(fixture.ctx, 5))
	assert.False(t, h.run)
	readDirHandle(t, fixture, h)
	h.Releasedir(fixture.ctx, 0)
	fixture.root.dir.mu.Lock()
	assert.Zero(t, fixture.root.dir.refills)
	fixture.root.dir.mu.Unlock()

	// Runs from offset 0 after the cache should have completed.
	for i := 1; i <= 4; i++ {
		h = openDirHandle(t, fixture, fixture.root, dirCacheFlags)
		entry, errno := h.Readdirent(fixture.ctx)
		require.Equal(t, syscall.Errno(0), errno)
		require.NotNil(t, entry)
		assert.True(t, h.run)
		h.Releasedir(fixture.ctx, 0)
		switch {
		case i < 3:
			assert.Zero(t, logged(), "after %d refills", i)
		default:
			assert.Equal(t, 1, logged(), "after %d refills", i)
		}
	}
	fixture.root.dir.mu.Lock()
	assert.True(t, fixture.root.dir.warned)
	fixture.root.dir.mu.Unlock()
}

func TestDirCacheFailedRename(t *testing.T) {
	ctx := context.Background()
	backing := t.TempDir()
	for _, name := range []string{"dir/a", "dir/m"} {
		fullPath := filepath.Join(backing, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0777))
		require.NoError(t, os.WriteFile(fullPath, []byte(name), 0666))
	}
	require.NoError(t, os.Mkdir(filepath.Join(backing, "other"), 0777))
	base, err := fs.NewFs(ctx, backing)
	require.NoError(t, err)

	fixture := newMount2NodeFixtureFs(t, base)
	fixture.fsys.dirCache = true
	dirVFS, err := fixture.dir.Stat("dir")
	require.NoError(t, err)
	dir, ok := dirVFS.(*vfs.Dir)
	require.True(t, ok, "node is %T", dirVFS)
	otherVFS, err := fixture.dir.Stat("other")
	require.NoError(t, err)
	other, ok := otherVFS.(*vfs.Dir)
	require.True(t, ok, "node is %T", otherVFS)
	dirNode := attachMount2Node(fixture.ctx, t, fixture.fsys, fixture.root, dir)

	generation, err := dir.Generation()
	require.NoError(t, err)

	// File.rename sets the new leaf, then the backend move fails because the
	// destination parent is a regular file.
	otherPath := filepath.Join(backing, "other")
	require.NoError(t, os.Remove(otherPath))
	require.NoError(t, os.WriteFile(otherPath, []byte("not a directory"), 0666))
	require.Error(t, dir.Rename("a", "z", other))

	h := openDirHandle(t, fixture, dirNode, dirCacheFlags)
	require.Len(t, h.entries, 4)
	assert.Equal(t, ".", h.entries[0].Name)
	assert.Equal(t, "..", h.entries[1].Name)
	assert.ElementsMatch(t, []string{"a", "m"}, entryNames(h.entries[2:]))
	for i, entry := range h.entries {
		assert.Equal(t, uint64(i+1), entry.Off, "entry %q", entry.Name)
	}
	key, _, _ := dirStateOf(t, dirNode)
	assert.Equal(t, generation, key.generation)
}

func TestDirCacheNotifyError(t *testing.T) {
	// openAfterChange returns a node whose cache holds an old listing, and
	// a change made after the last handle was released.
	openAfterChange := func(t *testing.T) (*mount2NodeFixture, dirKey, []fuse.DirEntry) {
		fixture := newDirCacheFixture(t, "a", "b")
		h := openDirHandle(t, fixture, fixture.root, dirCacheFlags)
		key, snap, _ := dirStateOf(t, fixture.root)
		h.Releasedir(fixture.ctx, 0)
		fixture.dir.AddVirtual("new", 1, false)
		return fixture, key, snap
	}

	t.Run("EIO", func(t *testing.T) {
		fixture, key, snap := openAfterChange(t)
		fixture.callbacks.setInodeStatus(fuse.EIO)

		h := openDirHandle(t, fixture, fixture.root, fuse.FOPEN_KEEP_CACHE)
		assert.False(t, h.caching)
		assert.Contains(t, entryNames(h.entries), "new")
		key2, snap2, caching := dirStateOf(t, fixture.root)
		assert.Equal(t, key, key2)
		assert.True(t, sameEntries(snap, snap2))
		assert.Zero(t, caching)
		assert.Equal(t, 1, fixture.callbacks.inodeCallCount())
		h.Releasedir(fixture.ctx, 0)

		fixture.callbacks.setInodeStatus(fuse.OK)
		h = openDirHandle(t, fixture, fixture.root, dirCacheFlags)
		assert.Equal(t, 2, fixture.callbacks.inodeCallCount())
		key3, snap3, _ := dirStateOf(t, fixture.root)
		assert.NotEqual(t, key.generation, key3.generation)
		assert.Contains(t, entryNames(snap3), "new")
		assert.True(t, sameEntries(snap3, h.entries))
	})

	t.Run("ENOENT", func(t *testing.T) {
		fixture, key, snap := openAfterChange(t)
		fixture.callbacks.setInodeStatus(fuse.ENOENT)

		h := openDirHandle(t, fixture, fixture.root, dirCacheFlags)
		assert.Equal(t, 1, fixture.callbacks.inodeCallCount())
		key2, snap2, _ := dirStateOf(t, fixture.root)
		assert.NotEqual(t, key.generation, key2.generation)
		assert.False(t, sameEntries(snap, snap2))
		assert.Contains(t, entryNames(snap2), "new")
		assert.True(t, sameEntries(snap2, h.entries))
	})
}

// fuseLog captures the go-fuse request log. go-fuse writes it to the
// standard logger because mount2 sets no MountOptions.Logger.
type fuseLog struct {
	mu    sync.Mutex
	lines []string
}

var readdirRequest = regexp.MustCompile(`\brx \d+: READDIR(PLUS)? n\d+\b`)

func (l *fuseLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, string(p))
	return len(p), nil
}

// readdirs returns the number of READDIR and READDIRPLUS requests seen.
func (l *fuseLog) readdirs() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, line := range l.lines {
		if readdirRequest.MatchString(line) {
			n++
		}
	}
	return n
}

func (l *fuseLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = nil
}

// newFUSELog sends the standard logger to a fuseLog until the test ends. It
// must run before the mount so its cleanup runs after the unmount.
func newFUSELog(t *testing.T) *fuseLog {
	t.Helper()
	l := &fuseLog{}
	old := stdlog.Writer()
	stdlog.SetOutput(l)
	t.Cleanup(func() { stdlog.SetOutput(old) })
	return l
}

// newDirCacheMount mounts files with FUSE request logging on.
func newDirCacheMount(t *testing.T, files map[string]string, adjust ...func(*mountlib.Options, *vfscommon.Options)) (*mount2LocalFixture, *fuseLog) {
	t.Helper()
	l := newFUSELog(t)
	debug := func(mountOpt *mountlib.Options, _ *vfscommon.Options) { mountOpt.DebugFUSE = true }
	return newMountedLocalFixture(t, files, append([]func(*mountlib.Options, *vfscommon.Options){debug}, adjust...)...), l
}

func withDirCacheTime(d time.Duration) func(*mountlib.Options, *vfscommon.Options) {
	return func(_ *mountlib.Options, vfsOpt *vfscommon.Options) { vfsOpt.DirCacheTime = fs.Duration(d) }
}

// requireEventuallyListed waits for name to be listed (or not) in dir.
func requireEventuallyListed(t *testing.T, dir, name string, listed bool) {
	t.Helper()
	require.Eventually(t, func() bool {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return false
		}
		return slices.ContainsFunc(entries, func(e os.DirEntry) bool { return e.Name() == name }) == listed
	}, 5*time.Second, 20*time.Millisecond, "%q listed = %v in %q", name, listed, dir)
}

func requireListed(t *testing.T, dir, name string, listed bool) {
	t.Helper()
	_, ok := requireDirNames(t, dir)[name]
	require.Equal(t, listed, ok, "%q listed in %q", name, dir)
}

func TestMount2DirCacheHit(t *testing.T) {
	f, l := newDirCacheMount(t, map[string]string{"file1": "1", "file2": "2"})

	requireDirNames(t, f.mount)
	require.Positive(t, l.readdirs(), "the first listing should read the directory")
	l.reset()

	names := requireDirNames(t, f.mount)
	assert.Contains(t, names, "file1")
	assert.Contains(t, names, "file2")
	assert.Zero(t, l.readdirs(), "the second listing should come from the kernel cache")
}

func TestMount2DirCacheThroughMount(t *testing.T) {
	// A long DirCacheTime leaves every change to the bumps of the VFS.
	f, _ := newDirCacheMount(t, map[string]string{"a": "a", "sub/x": "x"}, withDirCacheTime(time.Hour))
	sub := filepath.Join(f.mount, "sub")
	requireListed(t, f.mount, "a", true)
	requireListed(t, sub, "x", true)

	require.NoError(t, os.WriteFile(filepath.Join(f.mount, "new1"), []byte("new1"), 0666))
	requireListed(t, f.mount, "new1", true)
	require.NoError(t, os.Remove(filepath.Join(f.mount, "new1")))
	requireListed(t, f.mount, "new1", false)

	require.NoError(t, os.Mkdir(filepath.Join(f.mount, "dir1"), 0777))
	requireListed(t, f.mount, "dir1", true)
	require.NoError(t, os.Remove(filepath.Join(f.mount, "dir1")))
	requireListed(t, f.mount, "dir1", false)

	// Rename in the same parent
	require.NoError(t, os.Rename(filepath.Join(f.mount, "a"), filepath.Join(f.mount, "a2")))
	requireListed(t, f.mount, "a", false)
	requireListed(t, f.mount, "a2", true)

	// Rename across parents
	require.NoError(t, os.Rename(filepath.Join(f.mount, "a2"), filepath.Join(sub, "a3")))
	requireListed(t, f.mount, "a2", false)
	requireListed(t, sub, "a3", true)

	// Rename over an existing file
	require.NoError(t, os.WriteFile(filepath.Join(f.mount, "b"), []byte("b"), 0666))
	requireListed(t, f.mount, "b", true)
	require.NoError(t, os.Rename(filepath.Join(f.mount, "b"), filepath.Join(sub, "x")))
	requireListed(t, f.mount, "b", false)
	requireListed(t, sub, "x", true)
	got, err := os.ReadFile(filepath.Join(sub, "x"))
	require.NoError(t, err)
	assert.Equal(t, "b", string(got))
}

func TestMount2DirCacheExternalChange(t *testing.T) {
	f, _ := newDirCacheMount(t, map[string]string{"a": "a", "sub/x": "x"})
	sub := filepath.Join(f.mount, "sub")
	requireListed(t, f.mount, "a", true)
	requireListed(t, sub, "x", true)

	for _, dir := range []string{"", "sub"} {
		mountDir := filepath.Join(f.mount, dir)
		path := filepath.Join(f.backing, dir, "external")
		require.NoError(t, os.WriteFile(path, []byte("external"), 0666))
		requireEventuallyListed(t, mountDir, "external", true)
		require.NoError(t, os.Remove(path))
		requireEventuallyListed(t, mountDir, "external", false)
	}
}

func TestMount2DirCacheForgetRefresh(t *testing.T) {
	f, _ := newDirCacheMount(t, map[string]string{"a": "a", "d/x": "x"}, withDirCacheTime(time.Hour))
	d := filepath.Join(f.mount, "d")
	requireListed(t, f.mount, "a", true)
	requireListed(t, d, "x", true)

	call := func(method string, params rc.Params) {
		t.Helper()
		fn := rc.Calls.Get(method)
		require.NotNil(t, fn, method)
		params["fs"] = fs.ConfigString(f.mnt.VFS.Fs())
		_, err := fn.Fn(context.Background(), params)
		require.NoError(t, err, method)
	}
	external := func(rel string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(f.backing, filepath.FromSlash(rel)), []byte(rel), 0666))
	}

	for _, tc := range []struct {
		name   string
		method string
		params rc.Params
		file   string
		dir    string
	}{
		{"ForgetAll", "vfs/forget", rc.Params{}, "ext-all", f.mount},
		{"ForgetFile", "vfs/forget", rc.Params{"file": "d/ext-file"}, "d/ext-file", d},
		{"ForgetDir", "vfs/forget", rc.Params{"dir": "d"}, "d/ext-dir", d},
		{"Refresh", "vfs/refresh", rc.Params{}, "ext-refresh", f.mount},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// An earlier subtest may have invalidated tc.dir, so list it
			// to have the VFS cache it before the external write.
			requireDirNames(t, tc.dir)
			external(tc.file)
			leaf := filepath.Base(tc.file)
			requireListed(t, tc.dir, leaf, false)
			call(tc.method, tc.params)
			requireListed(t, tc.dir, leaf, true)
		})
	}
}

func TestMount2DirCacheZeroDirCacheTime(t *testing.T) {
	f, l := newDirCacheMount(t, map[string]string{"file1": "1"}, withDirCacheTime(0))

	requireDirNames(t, f.mount)
	require.Positive(t, l.readdirs(), "the first listing should read the directory")
	l.reset()
	names := requireDirNames(t, f.mount)
	assert.Contains(t, names, "file1")
	assert.Positive(t, l.readdirs(), "with --dir-cache-time 0 the cache is off, so an unchanged listing should read the directory")

	require.NoError(t, os.WriteFile(filepath.Join(f.backing, "external"), []byte("external"), 0666))
	requireEventuallyListed(t, f.mount, "external", true)
}

func TestMount2DirCachePartialRead(t *testing.T) {
	files := map[string]string{}
	for i := range 300 {
		files[fmt.Sprintf("file%03d", i)] = "x"
	}
	f, _ := newDirCacheMount(t, files)

	d, err := os.Open(f.mount)
	require.NoError(t, err)
	_, err = d.Readdirnames(10)
	require.NoError(t, err)
	require.NoError(t, d.Close())

	require.NoError(t, os.WriteFile(filepath.Join(f.backing, "external"), []byte("external"), 0666))
	requireEventuallyListed(t, f.mount, "external", true)

	want := requireDirNames(t, f.backing)
	for range 5 {
		assert.Equal(t, want, requireDirNames(t, f.mount))
	}
}

func TestMount2DirCacheHeldHandle(t *testing.T) {
	f, _ := newDirCacheMount(t, map[string]string{"a": "a", "b": "b", "c": "c"})

	held, err := os.Open(f.mount)
	require.NoError(t, err)
	defer func() { _ = held.Close() }()
	before, err := held.Readdirnames(-1)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"a", "b", "c"}, before)

	require.NoError(t, os.WriteFile(filepath.Join(f.backing, "external"), []byte("external"), 0666))
	requireEventuallyListed(t, f.mount, "external", true)

	_, err = held.Seek(0, io.SeekStart)
	require.NoError(t, err)
	after, err := held.Readdirnames(-1)
	require.NoError(t, err)
	assert.ElementsMatch(t, before, after, "a held handle should keep its listing across rewinddir")
}

// parseDirents returns the names in a getdents buffer, "." and ".." included.
func parseDirents(buf []byte) []string {
	var names []string
	for len(buf) > 0 {
		reclen := int(binary.NativeEndian.Uint16(buf[16:18]))
		name := buf[19:reclen]
		if i := slices.Index(name, 0); i >= 0 {
			name = name[:i]
		}
		names = append(names, string(name))
		buf = buf[reclen:]
	}
	return names
}

// getdentsAll reads fd to the end of the directory with a buffer of size
// bufSize and returns the names of the first call and of all calls.
func getdentsAll(t *testing.T, fd, bufSize int) (first, all []string) {
	t.Helper()
	buf := make([]byte, bufSize)
	for i := 0; ; i++ {
		n, err := unix.Getdents(fd, buf)
		require.NoError(t, err)
		if n == 0 {
			return first, all
		}
		names := parseDirents(buf[:n])
		if i == 0 {
			first = names
		}
		all = append(all, names...)
	}
}

func TestMount2DirCacheScanDuringChange(t *testing.T) {
	// 50 byte names make each cached entry 80 bytes (24 + 50 rounded up to
	// 8), which does not divide the 4096 byte page.
	files := map[string]string{}
	stable := make([]string, 2000)
	for i := range stable {
		stable[i] = fmt.Sprintf("f%04d%s", i, strings.Repeat("x", 45))
		files[stable[i]] = "x"
	}
	// A listing of 2,000 entries takes longer than the default fixture
	// DirCacheTime, which would expire the VFS listing in the middle of a scan.
	const dirCacheTime = time.Second
	f, l := newDirCacheMount(t, files, withDirCacheTime(dirCacheTime))

	// H1 reads the start of the directory and stays open.
	fd1, err := unix.Open(f.mount, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	require.NoError(t, err)
	defer func() { _ = unix.Close(fd1) }()
	buf := make([]byte, 1024)
	n, err := unix.Getdents(fd1, buf)
	require.NoError(t, err)
	first := parseDirents(buf[:n])
	require.Len(t, first, 15, "the first getdents should return . .. and 13 children")

	// The change, then H2 lists the directory while H1 is open.
	require.NoError(t, os.WriteFile(filepath.Join(f.backing, "zz-new"), []byte("new"), 0666))
	time.Sleep(dirCacheTime + 200*time.Millisecond)
	h2, err := os.Open(f.mount)
	require.NoError(t, err)
	h2Names, err := h2.Readdirnames(-1)
	require.NoError(t, err)
	assert.Contains(t, h2Names, "zz-new")

	// H1 reads to the end, and sees one listing: every name once, without the change.
	_, rest := getdentsAll(t, fd1, 1024)
	h1Names := slices.DeleteFunc(append(first, rest...), func(name string) bool { return name == "." || name == ".." })
	assert.NotContains(t, h1Names, "zz-new")
	slices.Sort(h1Names)
	assert.Equal(t, stable, h1Names, "H1 should list each name exactly once")
	require.NoError(t, unix.Close(fd1))
	require.NoError(t, h2.Close())

	// The first listing after both close fills the new cache. The next two are hits.
	assert.Contains(t, requireDirNames(t, f.mount), "zz-new")
	l.reset()
	assert.Contains(t, requireDirNames(t, f.mount), "zz-new")
	assert.Contains(t, requireDirNames(t, f.mount), "zz-new")
	assert.Zero(t, l.readdirs(), "the second and third listings should come from the kernel cache")
}

func TestMount2DirCacheFlagOff(t *testing.T) {
	f, l := newDirCacheMount(t, map[string]string{"file1": "1"}, func(mountOpt *mountlib.Options, _ *vfscommon.Options) {
		mountOpt.KernelDirCache = false
	})

	requireDirNames(t, f.mount)
	l.reset()
	requireDirNames(t, f.mount)
	assert.Positive(t, l.readdirs(), "with the cache off the second listing should read the directory")
}

func TestMount2DirCacheStress(t *testing.T) {
	const (
		stableCount = 2000
		churnTime   = 3 * time.Second
		cacheTime   = 200 * time.Millisecond
		readers     = 8
	)
	files := map[string]string{"gone": "gone"}
	stable := make([]string, stableCount)
	for i := range stable {
		stable[i] = fmt.Sprintf("stable-%04d", i)
		files[stable[i]] = "x"
	}
	f, l := newDirCacheMount(t, files, func(mountOpt *mountlib.Options, vfsOpt *vfscommon.Options) {
		mountOpt.AttrTimeout = fs.Duration(cacheTime)
		vfsOpt.DirCacheTime = fs.Duration(cacheTime)
	})

	// Warm-up: the second listing proves a complete cache.
	requireDirNames(t, f.mount)
	l.reset()
	requireDirNames(t, f.mount)
	require.Zero(t, l.readdirs(), "warm-up: the second listing should come from the kernel cache")

	// The permanent difference, never undone.
	require.NoError(t, os.WriteFile(filepath.Join(f.backing, "sentinel"), []byte("sentinel"), 0666))
	require.NoError(t, os.Remove(filepath.Join(f.backing, "gone")))

	// Every churn name is recorded before it is created.
	var (
		mu      sync.Mutex
		created = map[string]struct{}{}
	)
	known := func(name string) bool {
		if name == "gone" || name == "sentinel" || name == "." || name == ".." || strings.HasPrefix(name, "stable-") {
			return true
		}
		mu.Lock()
		defer mu.Unlock()
		_, ok := created[name]
		return ok
	}

	stop := make(chan struct{})
	var writer sync.WaitGroup
	writer.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
			name := fmt.Sprintf("churn-%d", i)
			mu.Lock()
			created[name] = struct{}{}
			mu.Unlock()
			path := filepath.Join(f.backing, name)
			if err := os.WriteFile(path, []byte(name), 0666); err != nil {
				t.Errorf("create %s: %v", name, err)
				return
			}
			if i%2 == 1 {
				if err := os.Remove(filepath.Join(f.backing, fmt.Sprintf("churn-%d", i-1))); err != nil {
					t.Errorf("remove churn-%d: %v", i-1, err)
					return
				}
			}
		}
	})

	var readerWG sync.WaitGroup
	for range readers {
		readerWG.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				fd, err := unix.Open(f.mount, unix.O_RDONLY|unix.O_DIRECTORY, 0)
				if err != nil {
					t.Errorf("open: %v", err)
					return
				}
				// One read in four stops after a random count.
				partial := rand.IntN(4) == 0
				stopAfter := 1 + rand.IntN(40)
				buf := make([]byte, 1024)
				var names []string
				complete := true
				for calls := 0; ; calls++ {
					if partial && calls == stopAfter {
						complete = false
						break
					}
					n, err := unix.Getdents(fd, buf)
					if err != nil {
						t.Errorf("getdents: %v", err)
						_ = unix.Close(fd)
						return
					}
					if n == 0 {
						break
					}
					names = append(names, parseDirents(buf[:n])...)
				}
				_ = unix.Close(fd)

				seen := make(map[string]int, len(names))
				for _, name := range names {
					seen[name]++
					if !known(name) {
						t.Errorf("listing has unknown name %q", name)
					}
				}
				for name, count := range seen {
					if count != 1 {
						t.Errorf("name %q listed %d times", name, count)
					}
				}
				if complete {
					for _, name := range stable {
						if seen[name] != 1 {
							t.Errorf("stable name %q listed %d times", name, seen[name])
							break
						}
					}
				}
			}
		})
	}

	time.Sleep(churnTime)
	close(stop)
	writer.Wait()
	readerWG.Wait()
	t.Logf("READDIR requests since warm-up: %d", l.readdirs())

	// The final listing equals the backing directory.
	time.Sleep(3 * cacheTime)
	got := requireDirNames(t, f.mount)
	assert.Contains(t, got, "sentinel")
	assert.NotContains(t, got, "gone")
	assert.Equal(t, requireDirNames(t, f.backing), got)
}
