//go:build linux

package mount2

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	fusefs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/cmd/mountlib"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fstest/mockfs"
	"github.com/rclone/rclone/fstest/mockobject"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/rclone/rclone/vfs/vfstest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestMount(t *testing.T) {
	vfstest.RunTests(t, false, vfscommon.CacheModeOff, true, mount)
}

type countingStatFs struct {
	fs.Fs

	mu    sync.Mutex
	stats map[string]int
}

func (f *countingStatFs) Stat(ctx context.Context, dir string, leaf string) (fs.DirEntry, error) {
	remote := leaf
	if dir != "" {
		remote = dir + "/" + leaf
	}

	f.mu.Lock()
	if f.stats == nil {
		f.stats = make(map[string]int)
	}
	f.stats[remote]++
	f.mu.Unlock()

	if stater, ok := f.Fs.(fs.Stater); ok {
		return stater.Stat(ctx, dir, leaf)
	}
	return f.Fs.NewObject(ctx, remote)
}

func (f *countingStatFs) statCount(remote string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stats[remote]
}

func requireNotifyPrune(t *testing.T) {
	t.Helper()

	opts := &fusefs.Options{}
	rawFS := fusefs.NewNodeFS(&fusefs.Inode{}, opts)
	server, err := fuse.NewServer(rawFS, t.TempDir(), &opts.MountOptions)
	require.NoError(t, err)
	mounted := true
	t.Cleanup(func() {
		if mounted {
			require.NoError(t, server.Unmount())
		}
	})
	go server.Serve()
	require.NoError(t, server.WaitMount())

	prune, _ := notifySupport(server.KernelSettings(), false)
	require.NoError(t, server.Unmount())
	mounted = false
	if !prune {
		t.Skip("NotifyPrune not supported by kernel")
	}
}

func TestMount2NotifyPruneForget(t *testing.T) {
	requireNotifyPrune(t)

	ctx := context.Background()

	baseFs, err := mockfs.NewFs(ctx, "mockfs", "root", nil)
	require.NoError(t, err)
	base := baseFs.(*mockfs.Fs)
	base.AddObject(mockobject.New("file1").WithContent([]byte("file1 contents"), mockobject.SeekModeRegular))

	countingFs := &countingStatFs{Fs: base}
	mountPoint := t.TempDir()

	mountOpt := mountlib.Opt
	mountOpt.AttrTimeout = fs.Duration(time.Minute)

	vfsOpt := vfscommon.Opt
	vfsOpt.DirCacheTime = fs.Duration(100 * time.Millisecond)

	mnt := mountlib.NewMountPoint(mount, mountPoint, countingFs, &mountOpt, &vfsOpt)
	mounted := false
	t.Cleanup(func() {
		if mounted {
			require.NoError(t, mnt.Unmount())
		}
	})

	_, err = mnt.Mount()
	require.NoError(t, err)
	mounted = true

	root, err := mnt.VFS.Root()
	require.NoError(t, err)

	filePath := filepath.Join(mountPoint, "file1")
	_, err = os.Stat(filePath)
	require.NoError(t, err)

	before := countingFs.statCount("file1")
	require.Positive(t, before)

	root.ForgetAll()

	_, err = os.Stat(filePath)
	require.NoError(t, err)

	require.Greater(t, countingFs.statCount("file1"), before)
}

func TestMount2NotifyPruneSharedVFS(t *testing.T) {
	requireNotifyPrune(t)

	ctx := context.Background()

	baseFs, err := mockfs.NewFs(ctx, "mockfs", "root", nil)
	require.NoError(t, err)
	base := baseFs.(*mockfs.Fs)
	base.AddObject(mockobject.New("fileA").WithContent([]byte("fileA contents"), mockobject.SeekModeRegular))
	base.AddObject(mockobject.New("fileB").WithContent([]byte("fileB contents"), mockobject.SeekModeRegular))

	countingFs := &countingStatFs{Fs: base}
	mountPointA := t.TempDir()
	mountPointB := t.TempDir()

	mountOpt := mountlib.Opt
	mountOpt.AttrTimeout = fs.Duration(time.Minute)

	vfsOpt := vfscommon.Opt
	vfsOpt.DirCacheTime = fs.Duration(100 * time.Millisecond)

	mntA := mountlib.NewMountPoint(mount, mountPointA, countingFs, &mountOpt, &vfsOpt)
	mntB := mountlib.NewMountPoint(mount, mountPointB, countingFs, &mountOpt, &vfsOpt)
	mountedA := false
	mountedB := false
	t.Cleanup(func() {
		if mountedB {
			require.NoError(t, mntB.Unmount())
		}
		if mountedA {
			require.NoError(t, mntA.Unmount())
		}
	})

	_, err = mntA.Mount()
	require.NoError(t, err)
	mountedA = true
	_, err = mntB.Mount()
	require.NoError(t, err)
	mountedB = true
	require.Same(t, mntA.VFS, mntB.VFS)

	root, err := mntA.VFS.Root()
	require.NoError(t, err)

	filePathA := filepath.Join(mountPointA, "fileA")
	_, err = os.Stat(filePathA)
	require.NoError(t, err)
	filePathB := filepath.Join(mountPointB, "fileB")
	_, err = os.Stat(filePathB)
	require.NoError(t, err)

	beforeA := countingFs.statCount("fileA")
	require.Positive(t, beforeA)
	beforeB := countingFs.statCount("fileB")
	require.Positive(t, beforeB)

	root.ForgetAll()

	_, err = os.Stat(filePathA)
	require.NoError(t, err)
	_, err = os.Stat(filePathB)
	require.NoError(t, err)

	require.Greater(t, countingFs.statCount("fileA"), beforeA)
	require.Greater(t, countingFs.statCount("fileB"), beforeB)
}

var _ fs.Stater = (*countingStatFs)(nil)
var _ vfs.Pruner = (*FS)(nil)

type mount2NodeFixture struct {
	ctx  context.Context
	fsys *FS
	root *Node
	vfs  *vfs.VFS
	dir  *vfs.Dir
	node map[string]*Node
}

func newMount2NodeFixture(t *testing.T, names ...string) *mount2NodeFixture {
	t.Helper()
	ctx := context.Background()

	baseFs, err := mockfs.NewFs(ctx, "mockfs", "root", nil)
	require.NoError(t, err)
	base := baseFs.(*mockfs.Fs)
	for _, name := range names {
		base.AddObject(mockobject.New(name).WithContent([]byte(name), mockobject.SeekModeRegular))
	}

	vfsOpt := vfscommon.Opt
	VFS := vfs.New(ctx, base, &vfsOpt)
	t.Cleanup(VFS.Shutdown)

	mountOpt := mountlib.Opt
	fsys := NewFS(VFS, &mountOpt)
	rootDir, err := VFS.Root()
	require.NoError(t, err)
	root := newNode(fsys, rootDir)
	fsys.root = root
	fusefs.NewNodeFS(root, &fusefs.Options{})

	fixture := &mount2NodeFixture{
		ctx:  ctx,
		fsys: fsys,
		root: root,
		vfs:  VFS,
		dir:  rootDir,
		node: make(map[string]*Node, len(names)),
	}
	for _, name := range names {
		vfsNode, err := rootDir.Stat(name)
		require.NoError(t, err)
		fixture.node[name] = attachMount2Node(ctx, t, fsys, root, vfsNode)
	}
	return fixture
}

func attachMount2Node(ctx context.Context, t *testing.T, fsys *FS, root *Node, vfsNode vfs.Node) *Node {
	t.Helper()
	node := newNode(fsys, vfsNode)
	var out fuse.EntryOut
	fsys.setEntryOut(vfsNode, &out)
	inode := root.NewInode(ctx, node, fusefs.StableAttr{Mode: out.Attr.Mode, Ino: vfsNode.Inode()})
	require.True(t, root.EmbeddedInode().AddChild(vfsNode.Name(), inode, false), "add child %q", vfsNode.Name())
	return node
}

func TestFSPruneCandidates(t *testing.T) {
	fixture := newMount2NodeFixture(t, "shared", "b-only", "unattached")

	mountOpt := mountlib.Opt
	fsysB := NewFS(fixture.vfs, &mountOpt)
	rootB := newNode(fsysB, fixture.dir)
	fsysB.root = rootB
	fusefs.NewNodeFS(rootB, &fusefs.Options{})

	shared, err := fixture.dir.Stat("shared")
	require.NoError(t, err)
	sharedB := attachMount2Node(fixture.ctx, t, fsysB, rootB, shared)
	foreign := &Node{}
	shared.SetSys(foreign)
	require.Same(t, fixture.node["shared"], fixture.fsys.nodeFor(shared))
	require.Same(t, sharedB, fsysB.nodeFor(shared))
	require.NotSame(t, foreign, fixture.fsys.nodeFor(shared))
	require.NotSame(t, foreign, fsysB.nodeFor(shared))

	bOnly, err := fixture.dir.Stat("b-only")
	require.NoError(t, err)
	bOnly.SetAux(fixture.fsys, nil)
	bOnlyB := attachMount2Node(fixture.ctx, t, fsysB, rootB, bOnly)
	bOnly.SetSys(foreign)
	require.Nil(t, fixture.fsys.nodeFor(bOnly))
	require.Same(t, bOnlyB, fsysB.nodeFor(bOnly))

	unattached, err := fixture.dir.Stat("unattached")
	require.NoError(t, err)
	unattached.SetAux(fixture.fsys, nil)
	unattachedA := newNode(fixture.fsys, unattached)
	var out fuse.EntryOut
	fixture.fsys.setEntryOut(unattached, &out)
	unattachedInode := fixture.root.NewInode(fixture.ctx, unattachedA, fusefs.StableAttr{
		Mode: out.Attr.Mode,
		Ino:  unattached.Inode(),
	})
	require.NotZero(t, unattachedInode.StableAttr().Ino)
	_, parent := unattachedInode.Parent()
	require.Nil(t, parent)

	ownedInode := fixture.node["shared"].EmbeddedInode()
	require.NotZero(t, ownedInode.StableAttr().Ino)
	_, parent = ownedInode.Parent()
	require.NotNil(t, parent)

	got := fixture.fsys.pruneCandidates([]vfs.Node{
		shared,
		shared,
		fixture.dir,
		bOnly,
		unattached,
	})
	require.Equal(t, []*fusefs.Inode{ownedInode}, got)

	got = fsysB.pruneCandidates([]vfs.Node{
		shared,
		shared,
		fixture.dir,
		bOnly,
		unattached,
	})
	require.Equal(t, []*fusefs.Inode{sharedB.EmbeddedInode(), bOnlyB.EmbeddedInode()}, got)
}

func TestNotifySupport(t *testing.T) {
	for _, test := range []struct {
		name        string
		settings    fuse.InitIn
		links       bool
		wantPrune   bool
		wantContent bool
	}{
		{
			name:     "no support",
			settings: fuse.InitIn{},
			links:    true,
		},
		{
			name: "before prune protocol",
			settings: fuse.InitIn{
				Major: 7,
				Minor: 44,
			},
		},
		{
			name: "prune protocol",
			settings: fuse.InitIn{
				Major: 7,
				Minor: 45,
			},
			wantPrune: true,
		},
		{
			name: "links disabled",
			settings: fuse.InitIn{
				Major: 7,
				Minor: 45,
				Flags: fuse.CAP_CACHE_SYMLINKS,
			},
			wantPrune: true,
		},
		{
			name: "cache symlinks unavailable",
			settings: fuse.InitIn{
				Major: 7,
				Minor: 45,
			},
			links:     true,
			wantPrune: true,
		},
		{
			name: "inode notification unavailable",
			settings: fuse.InitIn{
				Major: 7,
				Minor: 11,
				Flags: fuse.CAP_CACHE_SYMLINKS,
			},
			links: true,
		},
		{
			name: "content only",
			settings: fuse.InitIn{
				Major: 7,
				Minor: 12,
				Flags: fuse.CAP_CACHE_SYMLINKS,
			},
			links:       true,
			wantContent: true,
		},
		{
			name: "all support",
			settings: fuse.InitIn{
				Major: 7,
				Minor: 45,
				Flags: fuse.CAP_CACHE_SYMLINKS,
			},
			links:       true,
			wantPrune:   true,
			wantContent: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			prune, content := notifySupport(&test.settings, test.links)
			require.Equal(t, test.wantPrune, prune, "prune")
			require.Equal(t, test.wantContent, content, "content")
		})
	}
}

type foreignInodeNode struct {
	fusefs.Inode
}

var _ fusefs.InodeEmbedder = (*foreignInodeNode)(nil)

func TestNodeLookupChildFallback(t *testing.T) {
	for _, tc := range []struct {
		name      string
		setup     func(*mount2NodeFixture) *Node
		leaf      string
		wantName  string
		wantErrno syscall.Errno
	}{
		{
			name:     "success via dir.Stat",
			setup:    func(f *mount2NodeFixture) *Node { return f.root },
			leaf:     "file1",
			wantName: "file1",
		},
		{
			name: "vfsDir nil fallback",
			setup: func(f *mount2NodeFixture) *Node {
				f.root.vfsDir.Store(nil)
				return f.root
			},
			leaf:     "file1",
			wantName: "file1",
		},
		{
			name: "type assert miss leaves nil dir fallback",
			setup: func(f *mount2NodeFixture) *Node {
				return f.node["file1"]
			},
			wantName: "file1",
		},
		{
			name:      "error translation",
			setup:     func(f *mount2NodeFixture) *Node { return f.root },
			leaf:      "missing",
			wantErrno: syscall.ENOENT,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newMount2NodeFixture(t, "file1")
			node := tc.setup(fixture)
			got, errno := node.lookupChild(tc.leaf)
			require.Equal(t, tc.wantErrno, errno, "errno")
			if tc.wantErrno != 0 {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, tc.wantName, got.Name())
		})
	}
}

func TestNodeLookupSelfFallback(t *testing.T) {
	for _, tc := range []struct {
		name            string
		setup           func(*testing.T, *mount2NodeFixture) *Node
		wantName        string
		wantErrno       syscall.Errno
		wantErrFallback bool
	}{
		{
			name:     "root VFS Stat path",
			setup:    func(t *testing.T, f *mount2NodeFixture) *Node { return f.root },
			wantName: "/",
		},
		{
			name:     "success via parent lookupChild",
			setup:    func(t *testing.T, f *mount2NodeFixture) *Node { return f.node["file1"] },
			wantName: "file1",
		},
		{
			name: "parent nil fallback",
			setup: func(t *testing.T, f *mount2NodeFixture) *Node {
				child := f.node["file1"]
				ok, _ := f.root.EmbeddedInode().RmChild("file1")
				require.True(t, ok, "detach file1 child")
				_, parent := child.EmbeddedInode().Parent()
				require.Nil(t, parent, "detached child parent")
				require.NotSame(t, f.fsys.root, child, "detached child should not be root")
				return child
			},
			wantErrFallback: true,
		},
		{
			name: "parent non Node fallback",
			setup: func(t *testing.T, f *mount2NodeFixture) *Node {
				foreign := &foreignInodeNode{}
				foreignInode := f.root.NewInode(f.ctx, foreign, fusefs.StableAttr{Mode: fuse.S_IFDIR | 0755})
				require.True(t, f.root.EmbeddedInode().AddChild("foreign", foreignInode, false))
				vfsNode, err := f.dir.Stat("file1")
				require.NoError(t, err)
				child := newNode(f.fsys, vfsNode)
				var out fuse.EntryOut
				f.fsys.setEntryOut(vfsNode, &out)
				childInode := foreign.NewInode(f.ctx, child, fusefs.StableAttr{Mode: out.Attr.Mode})
				require.True(t, foreignInode.AddChild("file1", childInode, false))
				return child
			},
			wantErrFallback: true,
		},
		{
			name: "parent vfsDir nil chained fallback",
			setup: func(t *testing.T, f *mount2NodeFixture) *Node {
				f.root.vfsDir.Store(nil)
				return f.node["file1"]
			},
			wantName: "file1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newMount2NodeFixture(t, "file1")
			got, errno := tc.setup(t, fixture).lookupSelf()
			if tc.wantErrFallback {
				require.NotEqual(t, syscall.Errno(0), errno, "errno")
				assert.Nil(t, got)
				return
			}
			require.Equal(t, tc.wantErrno, errno, "errno")
			if tc.wantErrno != 0 {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, tc.wantName, got.Name())
		})
	}
}

type mount2LocalFixture struct {
	backing  string
	mount    string
	counting *countingStatFs
	mnt      *mountlib.MountPoint
}

func newMountedLocalFixture(t *testing.T, files map[string]string) *mount2LocalFixture {
	t.Helper()
	ctx := context.Background()
	backing := t.TempDir()
	for name, contents := range files {
		fullPath := filepath.Join(backing, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0777))
		require.NoError(t, os.WriteFile(fullPath, []byte(contents), 0666))
	}

	baseFs, err := fs.NewFs(ctx, backing)
	require.NoError(t, err)
	countingFs := &countingStatFs{Fs: baseFs}

	mountPoint := t.TempDir()
	mountOpt := mountlib.Opt
	mountOpt.AttrTimeout = fs.Duration(50 * time.Millisecond)
	vfsOpt := vfscommon.Opt
	vfsOpt.DirCacheTime = fs.Duration(50 * time.Millisecond)

	mnt := mountlib.NewMountPoint(mount, mountPoint, countingFs, &mountOpt, &vfsOpt)
	mounted := false
	t.Cleanup(func() {
		if mounted {
			require.NoError(t, mnt.Unmount())
		}
	})
	_, err = mnt.Mount()
	require.NoError(t, err)
	mounted = true

	return &mount2LocalFixture{
		backing:  backing,
		mount:    mountPoint,
		counting: countingFs,
		mnt:      mnt,
	}
}

func requireDirNames(t *testing.T, dir string) map[string]struct{} {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		names[entry.Name()] = struct{}{}
	}
	return names
}

func requireEventuallyNotExist(t *testing.T, filePath string) {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	var err error
	for {
		_, err = os.Stat(filePath)
		if os.IsNotExist(err) {
			return
		}
		if time.Now().After(deadline) {
			require.True(t, os.IsNotExist(err), "path %q error = %v", filePath, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func forgetMountedRoot(t *testing.T, f *mount2LocalFixture) {
	t.Helper()
	root, err := f.mnt.VFS.Root()
	require.NoError(t, err)
	root.ForgetAll()
}

func TestMount2LookupRenameRace(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*testing.T, *mount2LocalFixture)
	}{
		{
			name: "CHILD rename mid Lookup",
			run: func(t *testing.T, f *mount2LocalFixture) {
				require.NoError(t, os.Rename(filepath.Join(f.backing, "file1"), filepath.Join(f.backing, "file1_renamed")))
				forgetMountedRoot(t, f)
				requireEventuallyNotExist(t, filepath.Join(f.mount, "file1"))
				_, newErr := os.Stat(filepath.Join(f.mount, "file1_renamed"))
				require.NoError(t, newErr)
			},
		},
		{
			name: "SELF rename mid Getattr",
			run: func(t *testing.T, f *mount2LocalFixture) {
				file, err := os.Open(filepath.Join(f.mount, "file1"))
				require.NoError(t, err)
				defer func() { require.NoError(t, file.Close()) }()
				require.NoError(t, os.Rename(filepath.Join(f.backing, "file1"), filepath.Join(f.backing, "file1_renamed")))
				_, statErr := file.Stat()
				assert.NoError(t, statErr)
			},
		},
		{
			name: "parent rename mid Lookup",
			run: func(t *testing.T, f *mount2LocalFixture) {
				require.NoError(t, os.Rename(filepath.Join(f.backing, "dir1"), filepath.Join(f.backing, "dir2")))
				forgetMountedRoot(t, f)
				requireEventuallyNotExist(t, filepath.Join(f.mount, "dir1", "file1"))
				_, newErr := os.Stat(filepath.Join(f.mount, "dir2", "file1"))
				require.NoError(t, newErr)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.run(t, newMountedLocalFixture(t, map[string]string{
				"file1":      "file1",
				"dir1/file1": "nested",
			}))
		})
	}
}

func TestMount2OpendirHandleStaleness(t *testing.T) {
	fixture := newMount2NodeFixture(t, "file1", "file2", "file3")
	fh, _, errno := fixture.root.OpendirHandle(fixture.ctx, 0)
	require.Equal(t, syscall.Errno(0), errno, "OpendirHandle errno")
	dirHandle, ok := fh.(*mount2DirHandle)
	require.True(t, ok, "handle is %T", fh)
	defer dirHandle.Releasedir(fixture.ctx, 0)

	snapshotHasFile2 := false
	for _, entry := range dirHandle.entries {
		snapshotHasFile2 = snapshotHasFile2 || entry.Name == "file2"
	}
	require.True(t, snapshotHasFile2, "opened handle snapshot should contain file2")

	fixture.dir.DelVirtual("file2")

	var out fuse.EntryOut
	_, errno = dirHandle.Lookup(fixture.ctx, "file2", &out)
	require.Equal(t, syscall.ENOENT, errno, "handle Lookup should reflect external removal")
}

func TestMount2OpendirHandleVirtualEntries(t *testing.T) {
	fixture := newMountedLocalFixture(t, map[string]string{
		"file1": "file1",
	})
	require.NoError(t, fixture.mnt.VFS.AddVirtual("virtual-file", 12, false))

	names := requireDirNames(t, fixture.mount)
	require.Contains(t, names, "virtual-file")
	_, err := os.Stat(filepath.Join(fixture.mount, "virtual-file"))
	assert.NoError(t, err)
}

func TestMount2OpendirHandleConcurrent(t *testing.T) {
	fixture := newMountedLocalFixture(t, map[string]string{
		"dir/file1": "file1",
		"dir/file2": "file2",
		"dir/file3": "file3",
	})

	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 3 {
				names := requireDirNames(t, filepath.Join(fixture.mount, "dir"))
				if _, ok := names["file1"]; !ok {
					errs <- syscall.ENOENT
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		assert.NoError(t, err)
	}
}

func TestMountOptionsSyncRead(t *testing.T) {
	for _, tc := range []struct {
		name         string
		asyncRead    bool
		wantSyncRead bool
	}{
		{name: "async read enabled", asyncRead: true, wantSyncRead: false},
		{name: "async read disabled", asyncRead: false, wantSyncRead: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mountOpt := mountlib.Opt
			mountOpt.AsyncRead = tc.asyncRead
			fsys := &FS{
				VFS: &vfs.VFS{},
				opt: &mountOpt,
			}

			got := mountOptions(fsys, nil, &mountOpt)

			assert.Equal(t, tc.wantSyncRead, got.SyncRead)
		})
	}
}

func TestSetNegativeTimeout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout fs.Duration
		wantSet bool
	}{
		{name: "zero attr timeout", timeout: 0, wantSet: false},
		{name: "positive attr timeout", timeout: fs.Duration(time.Second), wantSet: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mountOpt := mountlib.Options{AttrTimeout: tc.timeout}
			opts := fusefs.Options{
				AttrTimeout: (*time.Duration)(&mountOpt.AttrTimeout),
			}

			setNegativeTimeout(&opts, &mountOpt.AttrTimeout)

			if !tc.wantSet {
				assert.Nil(t, opts.NegativeTimeout)
				return
			}
			require.NotNil(t, opts.NegativeTimeout)
			assert.True(t, opts.NegativeTimeout == opts.AttrTimeout, "NegativeTimeout should alias AttrTimeout")
			assert.Equal(t, *opts.AttrTimeout, *opts.NegativeTimeout)
		})
	}
}

// fakeVFSHandle is a minimal vfs.Handle stub used by passthrough fd tests. It
// returns a *vfs.File node for the FileHandle.PassthroughFd type guard and a
// caller-owned fd from each Fd call.
type fakeVFSHandle struct {
	vfs.Handle

	node vfs.Node
	fds  []int
}

func (h *fakeVFSHandle) Fd() uintptr {
	if len(h.fds) == 0 {
		return 0
	}
	fd := h.fds[0]
	h.fds = h.fds[1:]
	return uintptr(fd)
}

func (h *fakeVFSHandle) Node() vfs.Node { return h.node }

func (*fakeVFSHandle) Release() error { return nil }

func requireFdOpen(t *testing.T, fd int, msg string) {
	t.Helper()
	_, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	require.NoError(t, err, msg)
}

func assertFdClosed(t *testing.T, fd int, msg string) {
	t.Helper()
	_, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	assert.Error(t, err, msg)
}

// TestFileHandlePassthroughFdRecordsAndCloses verifies the fd lifecycle of
// FileHandle.PassthroughFd and FileHandle.Release:
//   - PassthroughFd records the fd returned by the vfs handle.
//   - A second PassthroughFd closes the prior recorded fd before replacement.
//   - Release closes the final fd exactly once via the -1 sentinel.
func TestFileHandlePassthroughFdRecordsAndCloses(t *testing.T) {
	tmp, err := os.CreateTemp(t.TempDir(), "fh_passthrough_test")
	require.NoError(t, err)
	dupFd1, err := syscall.Dup(int(tmp.Fd()))
	require.NoError(t, err)
	dupFd2, err := syscall.Dup(int(tmp.Fd()))
	require.NoError(t, err)
	require.NoError(t, tmp.Close())
	t.Cleanup(func() {
		_ = syscall.Close(dupFd1)
		_ = syscall.Close(dupFd2)
	})

	handle := &fakeVFSHandle{
		node: &vfs.File{},
		fds:  []int{dupFd1, dupFd2},
	}
	fh := newFileHandle(handle, nil)

	fd1, ok := fh.PassthroughFd()
	require.True(t, ok, "first PassthroughFd should succeed")
	assert.Equal(t, dupFd1, fd1)
	assert.Equal(t, fd1, fh.passthroughFd)
	requireFdOpen(t, fd1, "first passthrough fd should be open")

	fd2, ok := fh.PassthroughFd()
	require.True(t, ok, "second PassthroughFd should succeed")
	assert.Equal(t, dupFd2, fd2)
	assert.Equal(t, fd2, fh.passthroughFd)
	assertFdClosed(t, fd1, "prior passthrough fd should be closed after replacement")
	requireFdOpen(t, fd2, "second passthrough fd should be open")

	errno := fh.Release(context.Background())
	require.Equal(t, syscall.Errno(0), errno, "Release should succeed")
	assert.Equal(t, -1, fh.passthroughFd, "sentinel must be -1 after Release")
	assertFdClosed(t, fd2, "final passthrough fd should be closed after Release")

	errno2 := fh.Release(context.Background())
	require.Equal(t, syscall.Errno(0), errno2, "second Release should be a no-op")
}
