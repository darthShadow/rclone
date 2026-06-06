// Test suite for vfs

package vfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fstest"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Some times used in the tests
var (
	t1 = fstest.Time("2001-02-03T04:05:06.499999999Z")
	t2 = fstest.Time("2011-12-25T12:59:59.123456789Z")
	t3 = fstest.Time("2011-12-30T12:59:59.000000000Z")
)

// Constants uses in the tests
const (
	writeBackDelay      = fs.Duration(100 * time.Millisecond) // A short writeback delay for testing
	waitForWritersDelay = 30 * time.Second                    // time to wait for existing writers
)

type recordingPruner struct {
	mu    sync.Mutex
	calls [][]Node
}

func (p *recordingPruner) PruneInodes(victims []Node) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, append([]Node(nil), victims...))
}

func (p *recordingPruner) callsSnapshot() [][]Node {
	p.mu.Lock()
	defer p.mu.Unlock()

	calls := make([][]Node, len(p.calls))
	for i := range p.calls {
		calls[i] = append([]Node(nil), p.calls[i]...)
	}
	return calls
}

func requireSamePruners(t *testing.T, want, got []Pruner) {
	t.Helper()
	require.Len(t, got, len(want), "got pruners %v", got)
	matched := make([]bool, len(got))
	for _, wantPruner := range want {
		found := false
		for i, gotPruner := range got {
			if !matched[i] && gotPruner == wantPruner {
				matched[i] = true
				found = true
				break
			}
		}
		require.Truef(t, found, "missing pruner %T %p from snapshot %v", wantPruner, wantPruner, got)
	}
}

func containsSamePruner(pruners []Pruner, want Pruner) bool {
	for _, pruner := range pruners {
		if pruner == want {
			return true
		}
	}
	return false
}

func requireSameContentInvalidators(t *testing.T, want, got []ContentInvalidator) {
	t.Helper()
	require.Len(t, got, len(want), "got content invalidators %v", got)
	matched := make([]bool, len(got))
	for _, wantInvalidator := range want {
		found := false
		for i, gotInvalidator := range got {
			if !matched[i] && gotInvalidator == wantInvalidator {
				matched[i] = true
				found = true
				break
			}
		}
		require.Truef(t, found, "missing content invalidator %T %p from snapshot %v", wantInvalidator, wantInvalidator, got)
	}
}

func containsSameContentInvalidator(invalidators []ContentInvalidator, want ContentInvalidator) bool {
	for _, invalidator := range invalidators {
		if invalidator == want {
			return true
		}
	}
	return false
}

// TestMain drives the tests
func TestMain(m *testing.M) {
	fstest.TestMain(m)
}

// Clean up a test VFS
func cleanupVFS(t *testing.T, vfs *VFS) {
	vfs.WaitForWriters(waitForWritersDelay)
	err := vfs.CleanUp()
	require.NoError(t, err)
	vfs.Shutdown()
}

// Create a new VFS
func newTestVFSOpt(t *testing.T, opt *vfscommon.Options) (r *fstest.Run, vfs *VFS) {
	r = fstest.NewRun(t)
	vfs = New(context.Background(), r.Fremote, opt)
	t.Cleanup(func() {
		cleanupVFS(t, vfs)
	})
	return r, vfs
}

// Create a new VFS with default options
func newTestVFS(t *testing.T) (r *fstest.Run, vfs *VFS) {
	return newTestVFSOpt(t, nil)
}

func TestVFSPruner(t *testing.T) {
	var vfs VFS
	owner1, owner2, absentOwner := new(int), new(int), new(int)
	p1, p2, replacement := &recordingPruner{}, &recordingPruner{}, &recordingPruner{}

	require.Empty(t, vfs.pruners())

	for _, test := range []struct {
		name  string
		owner any
		set   Pruner
		want  []Pruner
	}{
		{
			name:  "SetFirstOwner",
			owner: owner1,
			set:   p1,
			want:  []Pruner{p1},
		},
		{
			name:  "SetSecondOwner",
			owner: owner2,
			set:   p2,
			want:  []Pruner{p1, p2},
		},
		{
			name:  "ReplaceFirstOwner",
			owner: owner1,
			set:   replacement,
			want:  []Pruner{replacement, p2},
		},
		{
			name:  "RemoveAbsentOwner",
			owner: absentOwner,
			want:  []Pruner{replacement, p2},
		},
		{
			name:  "RemoveFirstOwner",
			owner: owner1,
			want:  []Pruner{p2},
		},
		{
			name:  "RemoveLastOwner",
			owner: owner2,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			vfs.SetPruner(test.owner, test.set)
			requireSamePruners(t, test.want, vfs.pruners())
		})
	}

	vfs.SetPruner(owner1, p1)
	vfs.SetPruner(owner2, p2)
	snapshot := vfs.pruners()
	require.Len(t, snapshot, 2)
	snapshot[0] = nil
	requireSamePruners(t, []Pruner{p1, p2}, vfs.pruners())
}

func TestVFSPrunerConcurrent(t *testing.T) {
	const (
		owners     = 4
		iterations = 100
	)
	var (
		vfs VFS
		wg  sync.WaitGroup
	)
	for i := range owners {
		wg.Go(func() {
			owner := &i
			pruner := &recordingPruner{}
			for range iterations {
				vfs.SetPruner(owner, pruner)
				snapshot := vfs.pruners()
				assert.Truef(t, containsSamePruner(snapshot, pruner), "missing registered pruner %p from snapshot %v", pruner, snapshot)
				vfs.SetPruner(owner, nil)
			}
		})
	}
	wg.Wait()
	require.Empty(t, vfs.pruners())
}

func TestVFSContentInvalidator(t *testing.T) {
	var vfs VFS
	owner1, owner2, absentOwner := new(int), new(int), new(int)
	ci1, ci2, replacement := &recordingContentInvalidator{}, &recordingContentInvalidator{}, &recordingContentInvalidator{}

	require.Empty(t, vfs.contentInvalidators())

	for _, test := range []struct {
		name  string
		owner any
		set   ContentInvalidator
		want  []ContentInvalidator
	}{
		{
			name:  "SetFirstOwner",
			owner: owner1,
			set:   ci1,
			want:  []ContentInvalidator{ci1},
		},
		{
			name:  "SetSecondOwner",
			owner: owner2,
			set:   ci2,
			want:  []ContentInvalidator{ci1, ci2},
		},
		{
			name:  "ReplaceFirstOwner",
			owner: owner1,
			set:   replacement,
			want:  []ContentInvalidator{replacement, ci2},
		},
		{
			name:  "RemoveAbsentOwner",
			owner: absentOwner,
			want:  []ContentInvalidator{replacement, ci2},
		},
		{
			name:  "RemoveFirstOwner",
			owner: owner1,
			want:  []ContentInvalidator{ci2},
		},
		{
			name:  "RemoveLastOwner",
			owner: owner2,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			vfs.SetContentInvalidator(test.owner, test.set)
			requireSameContentInvalidators(t, test.want, vfs.contentInvalidators())
		})
	}

	vfs.SetContentInvalidator(owner1, ci1)
	vfs.SetContentInvalidator(owner2, ci2)
	snapshot := vfs.contentInvalidators()
	require.Len(t, snapshot, 2)
	snapshot[0] = nil
	requireSameContentInvalidators(t, []ContentInvalidator{ci1, ci2}, vfs.contentInvalidators())
}

func TestVFSContentInvalidatorConcurrent(t *testing.T) {
	const (
		owners     = 4
		iterations = 100
	)
	var (
		vfs VFS
		wg  sync.WaitGroup
	)
	for i := range owners {
		wg.Go(func() {
			owner := &i
			invalidator := &recordingContentInvalidator{}
			for range iterations {
				vfs.SetContentInvalidator(owner, invalidator)
				snapshot := vfs.contentInvalidators()
				assert.Truef(t, containsSameContentInvalidator(snapshot, invalidator), "missing registered content invalidator %p from snapshot %v", invalidator, snapshot)
				vfs.SetContentInvalidator(owner, nil)
			}
		})
	}
	wg.Wait()
	require.Empty(t, vfs.contentInvalidators())
}

// Check baseHandle performs as advertised
func TestVFSbaseHandle(t *testing.T) {
	fh := baseHandle{}

	err := fh.Chdir()
	assert.Equal(t, ENOSYS, err)

	err = fh.Chmod(0)
	assert.Equal(t, ENOSYS, err)

	err = fh.Chown(0, 0)
	assert.Equal(t, ENOSYS, err)

	err = fh.Close()
	assert.Equal(t, ENOSYS, err)

	fd := fh.Fd()
	assert.Equal(t, uintptr(0), fd)

	name := fh.Name()
	assert.Equal(t, "", name)

	_, err = fh.Read(nil)
	assert.Equal(t, ENOSYS, err)

	_, err = fh.ReadAt(nil, 0)
	assert.Equal(t, ENOSYS, err)

	_, err = fh.Readdir(0)
	assert.Equal(t, ENOSYS, err)

	_, err = fh.Readdirnames(0)
	assert.Equal(t, ENOSYS, err)

	_, err = fh.Seek(0, io.SeekStart)
	assert.Equal(t, ENOSYS, err)

	_, err = fh.Stat()
	assert.Equal(t, ENOSYS, err)

	err = fh.Sync()
	assert.Equal(t, nil, err)

	err = fh.Truncate(0)
	assert.Equal(t, ENOSYS, err)

	_, err = fh.Write(nil)
	assert.Equal(t, ENOSYS, err)

	_, err = fh.WriteAt(nil, 0)
	assert.Equal(t, ENOSYS, err)

	_, err = fh.WriteString("")
	assert.Equal(t, ENOSYS, err)

	err = fh.Flush()
	assert.Equal(t, ENOSYS, err)

	err = fh.Release()
	assert.Equal(t, ENOSYS, err)

	node := fh.Node()
	assert.Nil(t, node)
}

// TestVFSNew sees if the New command works properly
func TestVFSNew(t *testing.T) {
	// Check active cache has this many entries
	checkActiveCacheEntries := func(i int) {
		_, count := activeCacheEntries()
		assert.Equal(t, i, count)
	}

	checkActiveCacheEntries(0)

	r, vfs := newTestVFS(t)

	// Check making a VFS with nil options
	var defaultOpt = vfscommon.Opt
	defaultOpt.Init(context.Background())

	checkActiveCacheEntries(1)

	// Check that we get the same VFS if we ask for it again with
	// the same options
	vfs2 := New(context.Background(), r.Fremote, nil)
	assert.Equal(t, fmt.Sprintf("%p", vfs), fmt.Sprintf("%p", vfs2))

	checkActiveCacheEntries(1)

	// Shut the new VFS down and check the cache still has stuff in
	vfs2.Shutdown()

	checkActiveCacheEntries(1)

	cleanupVFS(t, vfs)

	checkActiveCacheEntries(0)
}

// TestVFSNewWithOpts sees if the New command works properly
func TestVFSNewWithOpts(t *testing.T) {
	var opt = vfscommon.Opt
	opt.DirPerms = 0777
	opt.FilePerms = 0666
	opt.Umask = 0002
	_, vfs := newTestVFSOpt(t, &opt)

	assert.Equal(t, vfscommon.FileMode(0775)|vfscommon.FileMode(os.ModeDir), vfs.Opt.DirPerms)
	assert.Equal(t, vfscommon.FileMode(0664), vfs.Opt.FilePerms)
}

// TestVFSRoot checks root directory is present and correct
func TestVFSRoot(t *testing.T) {
	_, vfs := newTestVFS(t)

	root, err := vfs.Root()
	require.NoError(t, err)
	assert.Equal(t, vfs.root, root)
	assert.True(t, root.IsDir())
	assert.Equal(t, os.FileMode(vfs.Opt.DirPerms).Perm(), root.Mode().Perm())
}

func TestVFSStat(t *testing.T) {
	r, vfs := newTestVFS(t)

	file1 := r.WriteObject(context.Background(), "file1", "file1 contents", t1)
	file2 := r.WriteObject(context.Background(), "dir/file2", "file2 contents", t2)
	r.CheckRemoteItems(t, file1, file2)

	node, err := vfs.Stat("file1")
	require.NoError(t, err)
	assert.True(t, node.IsFile())
	assert.Equal(t, "file1", node.Name())

	node, err = vfs.Stat("dir")
	require.NoError(t, err)
	assert.True(t, node.IsDir())
	assert.Equal(t, "dir", node.Name())

	node, err = vfs.Stat("dir/file2")
	require.NoError(t, err)
	assert.True(t, node.IsFile())
	assert.Equal(t, "file2", node.Name())

	_, err = vfs.Stat("not found")
	assert.Equal(t, os.ErrNotExist, err)

	_, err = vfs.Stat("dir/not found")
	assert.Equal(t, os.ErrNotExist, err)

	_, err = vfs.Stat("not found/not found")
	assert.Equal(t, os.ErrNotExist, err)

	_, err = vfs.Stat("file1/under a file")
	assert.Equal(t, os.ErrNotExist, err)
}

func TestVFSStatParent(t *testing.T) {
	r, vfs := newTestVFS(t)

	file1 := r.WriteObject(context.Background(), "file1", "file1 contents", t1)
	file2 := r.WriteObject(context.Background(), "dir/file2", "file2 contents", t2)
	r.CheckRemoteItems(t, file1, file2)

	node, leaf, err := vfs.StatParent("file1")
	require.NoError(t, err)
	assert.True(t, node.IsDir())
	assert.Equal(t, "/", node.Name())
	assert.Equal(t, "file1", leaf)

	node, leaf, err = vfs.StatParent("dir/file2")
	require.NoError(t, err)
	assert.True(t, node.IsDir())
	assert.Equal(t, "dir", node.Name())
	assert.Equal(t, "file2", leaf)

	node, leaf, err = vfs.StatParent("not found")
	require.NoError(t, err)
	assert.True(t, node.IsDir())
	assert.Equal(t, "/", node.Name())
	assert.Equal(t, "not found", leaf)

	_, _, err = vfs.StatParent("not found dir/not found")
	assert.Equal(t, os.ErrNotExist, err)

	_, _, err = vfs.StatParent("file1/under a file")
	assert.Equal(t, os.ErrExist, err)
}

func TestVFSOpenFile(t *testing.T) {
	r, vfs := newTestVFS(t)

	file1 := r.WriteObject(context.Background(), "file1", "file1 contents", t1)
	file2 := r.WriteObject(context.Background(), "dir/file2", "file2 contents", t2)
	r.CheckRemoteItems(t, file1, file2)

	fd, err := vfs.OpenFile("file1", os.O_RDONLY, 0777)
	require.NoError(t, err)
	assert.NotNil(t, fd)
	require.NoError(t, fd.Close())

	fd, err = vfs.OpenFile("dir", os.O_RDONLY, 0777)
	require.NoError(t, err)
	assert.NotNil(t, fd)
	require.NoError(t, fd.Close())

	fd, err = vfs.OpenFile("dir/new_file.txt", os.O_RDONLY, 0777)
	assert.Equal(t, os.ErrNotExist, err)
	assert.Nil(t, fd)

	fd, err = vfs.OpenFile("dir/new_file.txt", os.O_WRONLY|os.O_CREATE, 0777)
	require.NoError(t, err)
	assert.NotNil(t, fd)
	err = fd.Close()
	if !errors.Is(err, fs.ErrorCantUploadEmptyFiles) {
		require.NoError(t, err)
	}

	fd, err = vfs.OpenFile("not found/new_file.txt", os.O_WRONLY|os.O_CREATE, 0777)
	assert.Equal(t, os.ErrNotExist, err)
	assert.Nil(t, fd)
}

func TestVFSRename(t *testing.T) {
	r, vfs := newTestVFS(t)

	features := r.Fremote.Features()
	if features.Move == nil && features.Copy == nil {
		t.Skip("skip as can't rename files")
	}

	file1 := r.WriteObject(context.Background(), "dir/file2", "file2 contents", t2)
	r.CheckRemoteItems(t, file1)

	err := vfs.Rename("dir/file2", "dir/file1")
	require.NoError(t, err)
	file1.Path = "dir/file1"
	r.CheckRemoteItems(t, file1)

	err = vfs.Rename("dir/file1", "file0")
	require.NoError(t, err)
	file1.Path = "file0"
	r.CheckRemoteItems(t, file1)

	err = vfs.Rename("not found/file0", "file0")
	assert.Equal(t, os.ErrNotExist, err)

	err = vfs.Rename("file0", "not found/file0")
	assert.Equal(t, os.ErrNotExist, err)
}

func TestVFSStatfs(t *testing.T) {
	r, vfs := newTestVFS(t)

	// pre-conditions
	assert.Nil(t, vfs.usage)
	assert.True(t, vfs.usageTime.IsZero())

	aboutSupported := r.Fremote.Features().About != nil

	// read
	total, used, free := vfs.Statfs()
	if !aboutSupported {
		assert.Equal(t, int64(unknownFreeBytes), total)
		assert.Equal(t, int64(unknownFreeBytes), free)
		assert.Equal(t, int64(0), used)
		return // can't test anything else if About not supported
	}
	require.NotNil(t, vfs.usage)
	assert.False(t, vfs.usageTime.IsZero())
	if vfs.usage.Total != nil {
		assert.Equal(t, *vfs.usage.Total, total)
	} else {
		assert.True(t, total >= int64(unknownFreeBytes))
	}
	if vfs.usage.Free != nil {
		assert.Equal(t, *vfs.usage.Free, free)
	} else {
		if vfs.usage.Total != nil && vfs.usage.Used != nil {
			assert.Equal(t, free, total-used)
		} else {
			assert.True(t, free >= int64(unknownFreeBytes))
		}
	}
	if vfs.usage.Used != nil {
		assert.Equal(t, *vfs.usage.Used, used)
	} else {
		assert.Equal(t, int64(0), used)
	}

	// Validate IOBlockSize
	if vfs.usage.IOBlockSize != nil {
		assert.True(t, *vfs.usage.IOBlockSize > 0, "IOBlockSize should be positive")
		assert.True(t, *vfs.usage.IOBlockSize%512 == 0, "IOBlockSize should be a multiple of 512")
	}

	// read cached
	oldUsage := vfs.usage
	oldTime := vfs.usageTime
	total2, used2, free2 := vfs.Statfs()
	assert.Equal(t, oldUsage, vfs.usage)
	assert.Equal(t, total, total2)
	assert.Equal(t, used, used2)
	assert.Equal(t, free, free2)
	assert.Equal(t, oldTime, vfs.usageTime)
}

func TestVFSGetBlockSizes(t *testing.T) {
	r, vfs := newTestVFS(t)

	dataBlockSize, ioBlockSize := vfs.GetBlockSizes()

	// DataBlockSize should always be 512
	assert.Equal(t, int32(512), dataBlockSize, "DataBlockSize should always be 512")

	// IOBlockSize should be positive and a multiple of 512
	assert.True(t, ioBlockSize > 0, "IOBlockSize should be positive")
	assert.True(t, ioBlockSize%512 == 0, "IOBlockSize should be a multiple of 512")

	// If About is supported, IOBlockSize should match the usage
	if r.Fremote.Features().About != nil {
		// Call Statfs to populate usage
		_, _, _ = vfs.Statfs()

		if vfs.usage != nil && vfs.usage.IOBlockSize != nil {
			assert.Equal(t, *vfs.usage.IOBlockSize, ioBlockSize, "IOBlockSize should match usage.IOBlockSize")
		} else {
			// If no IOBlockSize in usage, should default to 4096
			assert.Equal(t, int32(4096), ioBlockSize, "IOBlockSize should default to 4096")
		}
	} else {
		// If About not supported, should default to 4096
		assert.Equal(t, int32(4096), ioBlockSize, "IOBlockSize should default to 4096 when About not supported")
	}

	// Test that multiple calls return the same values (caching)
	dataBlockSize2, ioBlockSize2 := vfs.GetBlockSizes()
	assert.Equal(t, dataBlockSize, dataBlockSize2, "DataBlockSize should be consistent")
	assert.Equal(t, ioBlockSize, ioBlockSize2, "IOBlockSize should be consistent")
}

func TestVFSMkdir(t *testing.T) {
	r, vfs := newTestVFS(t)

	if !r.Fremote.Features().CanHaveEmptyDirectories {
		return // can't test if can't have empty directories
	}

	r.CheckRemoteListing(t, nil, []string{})

	// Try making the root
	err := vfs.Mkdir("", 0777)
	require.NoError(t, err)
	r.CheckRemoteListing(t, nil, []string{})

	// Try making a sub directory
	err = vfs.Mkdir("a", 0777)
	require.NoError(t, err)

	r.CheckRemoteListing(t, nil, []string{"a"})

	// Try making an existing directory
	err = vfs.Mkdir("a", 0777)
	require.NoError(t, err)

	r.CheckRemoteListing(t, nil, []string{"a"})

	// Try making a new directory
	err = vfs.Mkdir("b/", 0777)
	require.NoError(t, err)

	r.CheckRemoteListing(t, nil, []string{"a", "b"})

	// Try making a new directory
	err = vfs.Mkdir("/c", 0777)
	require.NoError(t, err)

	r.CheckRemoteListing(t, nil, []string{"a", "b", "c"})

	// Try making a new directory
	err = vfs.Mkdir("/d/", 0777)
	require.NoError(t, err)

	r.CheckRemoteListing(t, nil, []string{"a", "b", "c", "d"})
}

func TestVFSMkdirAll(t *testing.T) {
	r, vfs := newTestVFS(t)

	if !r.Fremote.Features().CanHaveEmptyDirectories {
		return // can't test if can't have empty directories
	}

	r.CheckRemoteListing(t, nil, []string{})

	// Try making the root
	err := vfs.MkdirAll("", 0777)
	require.NoError(t, err)
	r.CheckRemoteListing(t, nil, []string{})

	// Try making a sub directory
	err = vfs.MkdirAll("a/b/c/d", 0777)
	require.NoError(t, err)

	r.CheckRemoteListing(t, nil, []string{"a", "a/b", "a/b/c", "a/b/c/d"})

	// Try making an existing directory
	err = vfs.MkdirAll("a/b/c", 0777)
	require.NoError(t, err)

	r.CheckRemoteListing(t, nil, []string{"a", "a/b", "a/b/c", "a/b/c/d"})

	// Try making an existing directory
	err = vfs.MkdirAll("/a/b/c/", 0777)
	require.NoError(t, err)

	r.CheckRemoteListing(t, nil, []string{"a", "a/b", "a/b/c", "a/b/c/d"})
}

func TestFillInMissingSizes(t *testing.T) {
	const unknownFree = 10
	for _, test := range []struct {
		total, free, used             int64
		wantTotal, wantUsed, wantFree int64
	}{
		{
			total: 20, free: 5, used: 15,
			wantTotal: 20, wantFree: 5, wantUsed: 15,
		},
		{
			total: 20, free: 5, used: -1,
			wantTotal: 20, wantFree: 5, wantUsed: 15,
		},
		{
			total: 20, free: -1, used: 15,
			wantTotal: 20, wantFree: 5, wantUsed: 15,
		},
		{
			total: 20, free: -1, used: -1,
			wantTotal: 20, wantFree: 20, wantUsed: 0,
		},
		{
			total: -1, free: 5, used: 15,
			wantTotal: 20, wantFree: 5, wantUsed: 15,
		},
		{
			total: -1, free: 15, used: -1,
			wantTotal: 15, wantFree: 15, wantUsed: 0,
		},
		{
			total: -1, free: -1, used: 15,
			wantTotal: 25, wantFree: 10, wantUsed: 15,
		},
		{
			total: -1, free: -1, used: -1,
			wantTotal: 10, wantFree: 10, wantUsed: 0,
		},
	} {
		t.Run(fmt.Sprintf("total=%d,free=%d,used=%d", test.total, test.free, test.used), func(t *testing.T) {
			gotTotal, gotUsed, gotFree := fillInMissingSizes(test.total, test.used, test.free, unknownFree)
			assert.Equal(t, test.wantTotal, gotTotal, "total")
			assert.Equal(t, test.wantUsed, gotUsed, "used")
			assert.Equal(t, test.wantFree, gotFree, "free")
		})
	}
}

func TestVFSIsMetadataFile(t *testing.T) {
	_, vfs := newTestVFS(t)

	rawName, found := vfs.isMetadataFile("leaf.metadata")
	assert.Equal(t, "leaf.metadata", rawName)
	assert.Equal(t, false, found)

	vfs.Opt.MetadataExtension = ".metadata"

	rawName, found = vfs.isMetadataFile("leaf.metadata")
	assert.Equal(t, "leaf", rawName)
	assert.Equal(t, true, found)
}
