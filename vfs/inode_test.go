package vfs

import (
	"context"
	"io"
	"os"
	"path"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/fs/operations"
	"github.com/rclone/rclone/fstest"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/require"
)

func TestVFSInodeRestart(t *testing.T) {
	opt := vfscommon.Opt
	opt.DirCacheTime = fs.Duration(time.Hour)
	opt.PollInterval = 0
	opt.CacheMode = vfscommon.CacheModeOff
	r, first := newTestVFSOpt(t, &opt)
	r.WriteObject(context.Background(), "top/sub/file", "unchanged contents", t1)
	opt.DirCacheTime += fs.Duration(time.Second)
	second := New(context.Background(), r.Fremote, &opt)
	t.Cleanup(func() { cleanupVFS(t, second) })
	require.NotSame(t, first, second)
	for _, name := range []string{"", "top", "top/sub", "top/sub/file"} {
		t.Run(name, func(t *testing.T) {
			a, err := first.Stat(name)
			require.NoError(t, err)
			b, err := second.Stat(name)
			require.NoError(t, err)
			require.Equal(t, a.Inode(), b.Inode(), "%s must keep its inode across VFS instances", name)
		})
	}
}

func TestInodeHashGolden(t *testing.T) {
	for _, test := range []struct {
		name, prefix, leaf string
		kind               byte
		id, want           uint64
	}{
		{"Root", "", "", 'd', 0, 5094309881604195654},
		{"DirWhole", "", "top/sub", 'd', 0, 5427041221133835121},
		{"DirSplit", "top", "sub", 'd', 0, 5427041221133835121},
		{"FileWhole", "", "top/sub/file", 'f', 0, 7614635513965552848},
		{"FileSplit", "top/sub", "file", 'f', 0, 7614635513965552848},
		{"Identity42", "top/sub", "file", 'f', 42, 8291885189832591556},
		{"Identity43", "top/sub", "file", 'f', 43, 624476522814434279},
		{"KindDir", "top/sub", "file", 'd', 0, 123625888896935790},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.want, inodePrefix(test.prefix).inode(test.leaf, test.kind, test.id))
		})
	}
}

func TestClampInode(t *testing.T) {
	for _, test := range []struct {
		name        string
		input, want uint64
	}{
		{"Zero", 0, 2}, {"One", 1, 3}, {"Max", 1<<63 - 1, 1<<63 - 2}, {"Bit63", 1<<63 | 5, 5},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := clampInode(test.input)
			require.Equal(t, test.want, r)
			require.GreaterOrEqual(t, r^(1<<63), uint64(1<<63))
			require.NotEqual(t, ^uint64(0), r^(1<<63))
		})
	}
}

func ageInodeClocks(t *testing.T, root *Dir, opt vfscommon.Options) {
	t.Helper()
	expired := time.Now().Add(-2 * time.Duration(opt.DirCacheTime))
	root.walk(func(dir *Dir) {
		if !dir.read.IsZero() {
			dir.read = expired
		}
		for leaf := range dir.statRead {
			dir.statRead[leaf] = expired
		}
		_, stale := dir._age(time.Now())
		require.True(t, stale)
	})
}

func refreshPath(t *testing.T, v *VFS, route, remote string) Node {
	t.Helper()
	if route == "ReadDirAll" {
		root, err := v.Root()
		require.NoError(t, err)
		_, err = root.ReadDirAll()
		require.NoError(t, err)
		parent := path.Dir(remote)
		if parent != "." {
			current := ""
			for _, part := range strings.Split(parent, "/") {
				current = path.Join(current, part)
				node, err := v.Stat(current)
				require.NoError(t, err)
				_, err = node.(*Dir).ReadDirAll()
				require.NoError(t, err)
			}
		}
	}
	node, err := v.Stat(remote)
	require.NoError(t, err)
	return node
}

func freshInode(t *testing.T, r *fstest.Run, opt vfscommon.Options, remote string) uint64 {
	t.Helper()
	opt.DirCacheTime += fs.Duration(time.Second)
	opt.CacheMode = vfscommon.CacheModeOff
	v := New(context.Background(), r.Fremote, &opt)
	t.Cleanup(func() { cleanupVFS(t, v) })
	node, err := v.Stat(remote)
	require.NoError(t, err)
	return node.Inode()
}

func listedIdentity(t *testing.T, f fs.Fs, remote string) uint64 {
	t.Helper()
	parent := path.Dir(remote)
	if parent == "." {
		parent = ""
	}
	entries, err := f.List(context.Background(), parent)
	require.NoError(t, err)
	var id uint64
	found := false
	for _, entry := range entries {
		if entry.Remote() == remote {
			found = true
			if e, ok := entry.(fs.EntryIdentifier); ok {
				id = e.EntryIdentity()
			}
			break
		}
	}
	require.True(t, found, "missing listed entry %q", remote)
	if id == 0 && *fstest.RemoteName == "" && runtime.GOOS != "windows" {
		t.Fatalf("default local entry %q must report a nonzero identity", remote)
	}
	return id
}

func replaceFileByMove(t *testing.T, r *fstest.Run, remote string) {
	t.Helper()
	ctx := context.Background()
	r.WriteObject(ctx, remote+".new", "replacement contents", t1)
	dst, err := r.Fremote.NewObject(ctx, remote)
	require.NoError(t, err)
	src, err := r.Fremote.NewObject(ctx, remote+".new")
	require.NoError(t, err)
	_, err = operations.Move(ctx, r.Fremote, dst, remote, src)
	require.NoError(t, err)
}

func replaceDirByMove(t *testing.T, r *fstest.Run, remote string) {
	t.Helper()
	ctx := context.Background()
	r.WriteObject(ctx, remote+".new/x", "replacement", t1)
	require.NoError(t, operations.Purge(ctx, r.Fremote, remote))
	require.NoError(t, r.Fremote.Features().DirMove(ctx, r.Fremote, remote+".new", remote))
}

func TestVFSInodeReplaced(t *testing.T) {
	for _, route := range []string{"ReadDirAll", "Stat"} {
		t.Run(route, func(t *testing.T) {
			for _, kind := range []string{"File", "Dir", "InPlace", "LinkDir"} {
				t.Run(kind, func(t *testing.T) {
					ctx := context.Background()
					opt := vfscommon.Opt
					opt.DirCacheTime = fs.Duration(time.Hour)
					opt.PollInterval = 0
					opt.CacheMode = vfscommon.CacheModeOff
					opt.Links = kind == "LinkDir"
					r, v := newTestVFSOpt(t, &opt)
					r.WriteObject(ctx, "top/file", "original contents", t1)
					r.WriteObject(ctx, "top/dir/x", "x", t1)
					remote := "top/file"
					if kind == "Dir" {
						remote = "top/dir"
						if r.Fremote.Features().DirMove == nil {
							t.Skip("backend has no DirMove")
						}
					} else if kind == "LinkDir" {
						r.WriteObject(ctx, "top/x.rclonelink/keep", "keep", t1)
						remote = "top/x"
					}
					before, err := v.Stat(remote)
					require.NoError(t, err)
					p := &recordingPruner{}
					v.SetPruner(p, p)
					t.Cleanup(func() { v.SetPruner(p, nil) })
					var h Handle
					switch kind {
					case "File":
						h, err = before.Open(os.O_RDONLY)
						require.NoError(t, err)
						b := make([]byte, 1)
						_, err = io.ReadFull(h, b)
						require.NoError(t, err)
						t.Cleanup(func() { require.NoError(t, h.Close()) })
						replaceFileByMove(t, r, remote)
					case "Dir":
						replaceDirByMove(t, r, remote)
					case "InPlace":
						id := listedIdentity(t, r.Fremote, remote)
						if id == 0 {
							t.Skip("backend has no file identity")
						}
						obj, err := r.Fremote.NewObject(ctx, remote)
						require.NoError(t, err)
						content := "in-place updated contents"
						info := object.NewStaticObjectInfo(remote, t1, int64(len(content)), true, nil, r.Fremote)
						require.NoError(t, obj.Update(ctx, strings.NewReader(content), info))
						afterID := listedIdentity(t, r.Fremote, remote)
						if afterID == 0 || id != afterID {
							t.Skip("backend replaces the entry on update")
						}
					}
					root, err := v.Root()
					require.NoError(t, err)
					ageInodeClocks(t, root, opt)
					after := refreshPath(t, v, route, remote)
					calls := p.callsSnapshot()
					require.Same(t, before, after, "refresh must retain the cached node")
					require.Equal(t, before.Inode(), after.Inode())
					if kind == "InPlace" {
						require.Empty(t, calls)
					}
					for _, call := range calls {
						require.NotContains(t, call, before)
					}
					if h != nil {
						_, err = io.ReadFull(h, make([]byte, 1))
						require.NoError(t, err)
					}
					v.FlushDirCache()
					rebuilt, err := v.Stat(remote)
					require.NoError(t, err)
					if kind == "File" || kind == "Dir" {
						if listedIdentity(t, r.Fremote, remote) == 0 {
							t.Skip("backend has no entry identity, so the rebuilt number cannot differ")
						}
						require.NotEqual(t, before.Inode(), rebuilt.Inode(), "number changes when the replaced entry is rebuilt")
					} else {
						require.Equal(t, before.Inode(), rebuilt.Inode())
					}
				})
			}
		})
	}
}

func TestVFSInodeRename(t *testing.T) {
	t.Run("RenameFile", func(t *testing.T) {
		opt := vfscommon.Opt
		opt.DirCacheTime = fs.Duration(time.Hour)
		opt.PollInterval = 0
		opt.CacheMode = vfscommon.CacheModeOff
		r, v := newTestVFSOpt(t, &opt)
		r.WriteObject(context.Background(), "top/a", "original contents", t1)
		top, err := v.Stat("top")
		require.NoError(t, err)
		_, err = top.(*Dir).ReadDirAll()
		require.NoError(t, err)
		n, err := v.Stat("top/a")
		require.NoError(t, err)
		initial := n.Inode()
		require.NoError(t, v.Rename("top/a", "top/b"))
		m, err := v.Stat("top/b")
		require.NoError(t, err)
		require.Same(t, n, m)
		require.Equal(t, n.Inode(), m.Inode())
		root, err := v.Root()
		require.NoError(t, err)
		ageInodeClocks(t, root, opt)
		m = refreshPath(t, v, "ReadDirAll", "top/b")
		require.Same(t, n, m)
		require.Equal(t, initial, m.Inode())
		want := inodePrefix("top").inode("b", inodeKindFile, listedIdentity(t, r.Fremote, "top/b"))
		require.Equal(t, want, freshInode(t, r, opt, "top/b"))
	})
	t.Run("MkdirLeftover", func(t *testing.T) {
		opt := vfscommon.Opt
		opt.DirCacheTime = fs.Duration(time.Hour)
		opt.PollInterval = 0
		opt.CacheMode = vfscommon.CacheModeOff
		opt.SkipMetadata = true
		r, v := newTestVFSOpt(t, &opt)
		if !r.Fremote.Features().CanHaveEmptyDirectories {
			t.Skip("backend cannot keep empty directories")
		}
		r.WriteObject(context.Background(), "top/keep", "keep", t1)
		node, err := v.Stat("top")
		require.NoError(t, err)
		top := node.(*Dir)
		_, err = top.ReadDirAll()
		require.NoError(t, err)
		_, err = top.Mkdir("A")
		require.NoError(t, err)
		require.NoError(t, v.Rename("top/A", "top/B"))
		a2, err := top.Mkdir("A")
		require.NoError(t, err)
		b, err := v.Stat("top/B")
		require.NoError(t, err)
		require.NotEqual(t, b.Inode(), a2.Inode(), "a created directory must not share the number of one renamed away")
		root, err := v.Root()
		require.NoError(t, err)
		ageInodeClocks(t, root, opt)
		after := refreshPath(t, v, "ReadDirAll", "top/B")
		require.Same(t, b, after)
	})
	t.Run("RenameDirKeepsNode", func(t *testing.T) {
		opt := vfscommon.Opt
		opt.DirCacheTime = fs.Duration(time.Hour)
		opt.PollInterval = 0
		opt.CacheMode = vfscommon.CacheModeOff
		r, v := newTestVFSOpt(t, &opt)
		r.WriteObject(context.Background(), "top/d/file", "original contents", t1)
		d, err := v.Stat("top/d")
		require.NoError(t, err)
		initial := d.Inode()
		require.NoError(t, v.Rename("top/d", "top/e"))
		root, err := v.Root()
		require.NoError(t, err)
		ageInodeClocks(t, root, opt)
		after := refreshPath(t, v, "ReadDirAll", "top/e")
		require.Same(t, d, after)
		require.Equal(t, initial, after.Inode())
	})
}

func TestVFSInodeCreated(t *testing.T) {
	opt := vfscommon.Opt
	opt.DirCacheTime = fs.Duration(time.Hour)
	opt.PollInterval = 0
	opt.CacheMode = vfscommon.CacheModeWrites
	opt.WriteBack = writeBackDelay
	r, v := newTestVFSOpt(t, &opt)
	r.WriteObject(context.Background(), "top/keep", "keep", t1)
	h, err := v.OpenFile("top/new", os.O_CREATE|os.O_WRONLY, 0600)
	require.NoError(t, err)
	_, err = h.Write([]byte("new contents"))
	require.NoError(t, err)
	require.NoError(t, h.Close())
	node, err := v.Stat("top/new")
	require.NoError(t, err)
	initial := node.Inode()
	require.NotEqual(t, inodePrefix("top").inode("new", inodeKindFile, 0), initial)
	v.WaitForWriters(waitForWritersDelay)
	root, err := v.Root()
	require.NoError(t, err)
	ageInodeClocks(t, root, opt)
	after := refreshPath(t, v, "Stat", "top/new")
	require.Same(t, node, after, "refresh must retain the created node after upload")
	require.Equal(t, initial, after.Inode())
	v.FlushDirCache()
	node, err = v.Stat("top/new")
	require.NoError(t, err)
	uploaded := node.Inode()
	require.Equal(t, freshInode(t, r, opt, "top/new"), uploaded)
	require.NotEqual(t, initial, uploaded)
	v.FlushDirCache()
	node, err = v.Stat("top/new")
	require.NoError(t, err)
	require.Equal(t, uploaded, node.Inode())
	ageInodeClocks(t, root, opt)
	node = refreshPath(t, v, "ReadDirAll", "top/new")
	require.Equal(t, uploaded, node.Inode())
}

func TestVFSInodeCreatedRenamed(t *testing.T) {
	opt := vfscommon.Opt
	opt.DirCacheTime = fs.Duration(time.Hour)
	opt.PollInterval = 0
	opt.CacheMode = vfscommon.CacheModeWrites
	opt.WriteBack = writeBackDelay
	r, v := newTestVFSOpt(t, &opt)
	features := r.Fremote.Features()
	if features.Move == nil && features.Copy == nil {
		t.Skip("remote supports neither server-side move nor copy for VFS rename")
	}
	r.WriteObject(context.Background(), "top/keep", "keep", t1)
	for i, content := range []string{"first contents", "second different contents"} {
		h, err := v.OpenFile("top/a", os.O_CREATE|os.O_WRONLY, 0600)
		require.NoError(t, err)
		_, err = h.Write([]byte(content))
		require.NoError(t, err)
		require.NoError(t, h.Close())
		v.WaitForWriters(waitForWritersDelay)
		if i == 0 {
			require.NoError(t, v.Rename("top/a", "top/b"))
		}
	}
	a, err := v.Stat("top/a")
	require.NoError(t, err)
	b, err := v.Stat("top/b")
	require.NoError(t, err)
	require.NotEqual(t, a.Inode(), b.Inode(), "created and renamed files must have distinct inodes")
	root, err := v.Root()
	require.NoError(t, err)
	ageInodeClocks(t, root, opt)
	a = refreshPath(t, v, "ReadDirAll", "top/a")
	b = refreshPath(t, v, "ReadDirAll", "top/b")
	require.NotEqual(t, a.Inode(), b.Inode(), "refresh must keep created and renamed files distinct")
}

// otherRemoteFs reports directory "m" at a different remote, as a
// backend does when a name does not survive its encoding round trip.
type otherRemoteFs struct{ fs.Fs }

func (f *otherRemoteFs) Stat(ctx context.Context, dir, leaf string) (fs.DirEntry, error) {
	if leaf == "m" {
		return fs.NewDir(path.Join(dir, "m-encoded"), t1), nil
	}
	stater, ok := f.Fs.(fs.Stater)
	if !ok {
		return nil, fs.ErrorNotImplemented
	}
	return stater.Stat(ctx, dir, leaf)
}

func TestVFSInodeMkdirKeepsPath(t *testing.T) {
	ctx := context.Background()
	opt := vfscommon.Opt
	opt.DirCacheTime = fs.Duration(time.Hour)
	opt.PollInterval = 0
	opt.CacheMode = vfscommon.CacheModeOff
	opt.SkipMetadata = true
	r := fstest.NewRun(t)
	r.WriteObject(ctx, "top/keep", "keep", t1)
	v := New(ctx, &otherRemoteFs{Fs: r.Fremote}, &opt)
	t.Cleanup(func() { cleanupVFS(t, v) })
	node, err := v.Stat("top")
	require.NoError(t, err)
	d, err := node.(*Dir).Mkdir("m")
	require.NoError(t, err)
	require.Equal(t, "top/m", d.Path())
	require.IsType(t, &fs.Dir{}, d.DirEntry())
	_, err = v.Stat("top/m")
	require.NoError(t, err)
	fd, err := v.OpenFile("top/m/file", os.O_WRONLY|os.O_CREATE, 0600)
	require.NoError(t, err)
	require.NoError(t, fd.Close())
}

func TestVFSInodeIdentityOff(t *testing.T) {
	for _, kind := range []string{"ListedFile", "ReplacedKeeps"} {
		t.Run(kind, func(t *testing.T) {
			opt := vfscommon.Opt
			opt.DirCacheTime = fs.Duration(time.Hour)
			opt.PollInterval = 0
			opt.CacheMode = vfscommon.CacheModeOff
			opt.NoEntryIdentity = true
			r, v := newTestVFSOpt(t, &opt)
			r.WriteObject(context.Background(), "top/file", "original contents", t1)
			top, err := v.Stat("top")
			require.NoError(t, err)
			if kind == "ListedFile" {
				_, err = top.(*Dir).ReadDirAll()
				require.NoError(t, err)
			}
			before, err := v.Stat("top/file")
			require.NoError(t, err)
			if kind == "ListedFile" {
				require.Equal(t, inodePrefix("top").inode("file", inodeKindFile, 0), before.Inode())
			} else {
				replaceFileByMove(t, r, "top/file")
				root, err := v.Root()
				require.NoError(t, err)
				ageInodeClocks(t, root, opt)
				after := refreshPath(t, v, "Stat", "top/file")
				require.Equal(t, before.Inode(), after.Inode())
				v.FlushDirCache()
				after, err = v.Stat("top/file")
				require.NoError(t, err)
				require.Equal(t, before.Inode(), after.Inode())
			}
		})
	}
}
