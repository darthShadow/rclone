//go:build linux

package mount2

import (
	"context"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/rclone/rclone/fstest/mockfs"
	"github.com/rclone/rclone/fstest/mockobject"
	"github.com/rclone/rclone/vfs"
	"github.com/stretchr/testify/require"
)

func TestMount2LookupAfterFlushGetsNewGen(t *testing.T) {
	fixture := newMount2NodeFixture(t)
	fixture.vfs.Fs().(*mockfs.Fs).AddObject(mockobject.New("file1").WithContent([]byte("file1"), mockobject.SeekModeRegular))
	header := &fuse.InHeader{NodeId: fuse.FUSE_ROOT_ID}
	var out1, out2 fuse.EntryOut
	require.Equal(t, fuse.OK, fixture.rawFS.Lookup(context.Background(), header, "file1", &out1))
	fixture.vfs.FlushDirCache()
	require.Equal(t, fuse.OK, fixture.rawFS.Lookup(context.Background(), header, "file1", &out2))
	require.Equal(t, out1.Attr.Ino, out2.Attr.Ino)
	require.NotEqual(t, out1.NodeId, out2.NodeId, "rebuilt VFS nodes must get different go-fuse nodes")
	require.NotEqual(t, out1.Generation, out2.Generation)
	newNode, err := fixture.dir.Stat("file1")
	require.NoError(t, err)
	require.Len(t, fixture.fsys.pruneCandidates([]vfs.Node{newNode}), 1)
}
