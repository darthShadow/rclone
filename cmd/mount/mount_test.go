//go:build linux

package mount

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"bazil.org/fuse"
	fusefs "bazil.org/fuse/fs"
	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/cmd/mountlib"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fstest/mockfs"
	"github.com/rclone/rclone/fstest/mockobject"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/rclone/rclone/vfs/vfstest"
)

func TestMount(t *testing.T) {
	vfstest.RunTests(t, false, vfscommon.CacheModeOff, true, mount)
}

func newMountFileFixture(t *testing.T) (*FS, *vfs.File) {
	t.Helper()
	ctx := context.Background()
	baseFs, err := mockfs.NewFs(ctx, "mockfs", "root", nil)
	if err != nil {
		t.Fatalf("NewFs() error = %v", err)
	}
	base := baseFs.(*mockfs.Fs)
	base.AddObject(mockobject.New("file1").WithContent([]byte("file1"), mockobject.SeekModeRegular))

	vfsOpt := vfscommon.Opt
	VFS := vfs.New(ctx, base, &vfsOpt)
	t.Cleanup(VFS.Shutdown)
	root, err := VFS.Root()
	if err != nil {
		t.Fatalf("Root() error = %v", err)
	}
	vfsNode, err := root.Stat("file1")
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	vfsFile, ok := vfsNode.(*vfs.File)
	if !ok {
		t.Fatalf("Stat() node type = %T, want *vfs.File", vfsNode)
	}
	return NewFS(VFS, &mountlib.Opt), vfsFile
}

func newMountSymlinkFixture(t *testing.T) *FS {
	t.Helper()
	ctx := context.Background()
	baseFs, err := fs.NewFs(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("NewFs() error = %v", err)
	}
	shutdown := baseFs.Features().Shutdown
	if shutdown == nil {
		t.Fatal("local backend has no Shutdown feature")
	}
	t.Cleanup(func() {
		if err := shutdown(context.Background()); err != nil {
			t.Errorf("backend Shutdown() error = %v", err)
		}
	})
	vfsOpt := vfscommon.Opt
	vfsOpt.Links = true
	VFS := vfs.New(ctx, baseFs, &vfsOpt)
	t.Cleanup(VFS.Shutdown)
	return NewFS(VFS, &mountlib.Opt)
}

type synchronizedAuxNode struct {
	*vfs.File

	attachAttempt chan struct{}
	releaseAttach chan struct{}
}

func (n *synchronizedAuxNode) waitForAttach() {
	n.attachAttempt <- struct{}{}
	<-n.releaseAttach
}

func (n *synchronizedAuxNode) SetAux(owner, value any) {
	n.waitForAttach()
	n.File.SetAux(owner, value)
}

func (n *synchronizedAuxNode) LoadOrStoreAux(owner, value any) (actual any, loaded bool) {
	n.waitForAttach()
	return n.File.LoadOrStoreAux(owner, value)
}

func TestNewNodeConcurrentFirstLookupReturnsSameNode(t *testing.T) {
	fsys, vfsFile := newMountFileFixture(t)
	wrapped := &synchronizedAuxNode{
		File:          vfsFile,
		attachAttempt: make(chan struct{}, 2),
		releaseAttach: make(chan struct{}),
	}

	candidates := []fusefs.Node{
		&File{vfsFile, fsys},
		&File{vfsFile, fsys},
	}
	nodes := make([]fusefs.Node, 2)
	panics := make(chan any, len(nodes))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range nodes {
		wg.Go(func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					panics <- recovered
				}
			}()
			<-start
			nodes[i] = attachNode(fsys, wrapped, candidates[i])
		})
	}
	close(start)
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() { close(wrapped.releaseAttach) })
	}
	defer release()
	for attempt := range 2 {
		select {
		case <-wrapped.attachAttempt:
		case <-time.After(time.Second):
			t.Fatalf("attachment attempts = %d, want 2", attempt)
		}
	}
	release()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("attachment goroutines did not finish")
	}
	close(panics)
	hadPanic := false
	for recovered := range panics {
		hadPanic = true
		t.Errorf("attachment panicked: %v", recovered)
	}
	if hadPanic {
		return
	}

	if nodes[0] != nodes[1] {
		t.Errorf("concurrent first lookups returned %p and %p, want the same node", nodes[0], nodes[1])
	}
	if actual := wrapped.Aux(fsys); actual != nodes[0] {
		t.Errorf("attached node = %p, want returned node %p", actual, nodes[0])
	}
}

func TestNewNodeUnexpectedAuxValueReturnsCandidate(t *testing.T) {
	fsys, vfsFile := newMountFileFixture(t)
	unexpected := new(int)
	vfsFile.SetAux(fsys, unexpected)

	node := newNode(fsys, vfsFile)
	if _, ok := node.(*File); !ok {
		t.Errorf("newNode() type = %T, want *File", node)
	}
	if actual := vfsFile.Aux(fsys); actual != unexpected {
		t.Errorf("attached value = %T %p, want unexpected value %p preserved", actual, actual, unexpected)
	}
}

func TestCreateExistingKeepsLookupNode(t *testing.T) {
	fsys, vfsFile := newMountFileFixture(t)
	ctx := context.Background()
	rootNode, err := fsys.Root()
	if err != nil {
		t.Fatalf("Root() error = %v", err)
	}
	rootDir, ok := rootNode.(*Dir)
	if !ok {
		t.Fatalf("Root() node type = %T, want *Dir", rootNode)
	}

	var lookupResp fuse.LookupResponse
	lookedUp, err := rootDir.Lookup(ctx, &fuse.LookupRequest{Name: "file1"}, &lookupResp)
	if err != nil {
		t.Fatalf("Lookup() error = %v", err)
	}

	var createResp fuse.CreateResponse
	created, handle, err := rootDir.Create(ctx, &fuse.CreateRequest{
		Name:  "file1",
		Flags: fuse.OpenFlags(os.O_WRONLY),
	}, &createResp)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	fileHandle, ok := handle.(*FileHandle)
	if !ok {
		t.Fatalf("Create() handle type = %T, want *FileHandle", handle)
	}

	if created != lookedUp {
		t.Errorf("Create() node = %p, want looked-up node %p", created, lookedUp)
	}
	if actual := vfsFile.Aux(fsys); actual != lookedUp {
		t.Errorf("attached node = %p, want looked-up node %p", actual, lookedUp)
	}
	if err := fileHandle.Release(ctx, &fuse.ReleaseRequest{}); err != nil {
		t.Errorf("Release() error = %v", err)
	}
}

func TestRootReturnsAttachedNode(t *testing.T) {
	fsys, _ := newMountFileFixture(t)

	first, err := fsys.Root()
	if err != nil {
		t.Fatalf("first Root() error = %v", err)
	}
	second, err := fsys.Root()
	if err != nil {
		t.Fatalf("second Root() error = %v", err)
	}

	if first != second {
		t.Errorf("Root() nodes = %p and %p, want the same node", first, second)
	}
	root, err := fsys.VFS.Root()
	if err != nil {
		t.Fatalf("VFS Root() error = %v", err)
	}
	if actual := root.Aux(fsys); actual != first {
		t.Errorf("attached root node = %p, want returned node %p", actual, first)
	}
}

func TestSymlinkAttachesReturnedNode(t *testing.T) {
	fsys := newMountSymlinkFixture(t)
	ctx := context.Background()
	rootNode, err := fsys.Root()
	if err != nil {
		t.Fatalf("Root() error = %v", err)
	}
	rootDir, ok := rootNode.(*Dir)
	if !ok {
		t.Fatalf("Root() node type = %T, want *Dir", rootNode)
	}

	created, err := rootDir.Symlink(ctx, &fuse.SymlinkRequest{NewName: "link", Target: "target"})
	if err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	createdFile, ok := created.(*File)
	if !ok {
		t.Fatalf("Symlink() node type = %T, want *File", created)
	}
	if actual := createdFile.File.Aux(fsys); actual != created {
		t.Errorf("attached symlink node = %p, want returned node %p", actual, created)
	}
	var lookupResp fuse.LookupResponse
	lookedUp, err := rootDir.Lookup(ctx, &fuse.LookupRequest{Name: "link"}, &lookupResp)
	if err != nil {
		t.Fatalf("Lookup() error = %v", err)
	}
	if lookedUp != created {
		t.Errorf("Lookup() node = %p, want created node %p", lookedUp, created)
	}
}
