//go:build linux

package mount2

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	fusefs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/rclone/rclone/cmd/mountlib"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fstest/mockfs"
	"github.com/rclone/rclone/fstest/mockobject"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/rclone/rclone/vfs/vfstest"
	"github.com/stretchr/testify/require"
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
