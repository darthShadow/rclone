//go:build linux || (darwin && amd64)

package mount2

import (
	"context"
	"syscall"

	fusefs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/rclone/rclone/vfs"
)

// mount2DirHandle is the per-open directory handle returned by OpendirHandle.
// It keeps the opened directory identity and a listing snapshot, but does not
// cache child vfs.Node values. READDIRPLUS lookups re-resolve each name through
// dir.Stat so VFS owns namespace freshness.
//
// concurrency: go-fuse's bridge file-entry mutex serializes Readdirent,
// Lookup, and Seekdir callbacks within ReadDir and ReadDirPlus requests.
type mount2DirHandle struct {
	parent  *Node
	dir     *vfs.Dir
	entries []fuse.DirEntry
	index   int
}

var _ fusefs.FileHandle = (*mount2DirHandle)(nil)
var _ fusefs.FileReaddirenter = (*mount2DirHandle)(nil)
var _ fusefs.FileLookuper = (*mount2DirHandle)(nil)
var _ fusefs.FileReleasedirer = (*mount2DirHandle)(nil)
var _ fusefs.FileSeekdirer = (*mount2DirHandle)(nil)

// vfsNodeToDirEntry converts VFS nodes to FUSE directory entries.
func vfsNodeToDirEntry(n vfs.Node) (fuse.DirEntry, error) {
	return fuse.DirEntry{
		Mode: getMode(n),
		Name: n.Name(),
		Ino:  n.Inode(),
	}, nil
}

// OpendirHandle implements fusefs.NodeOpendirHandler. It resolves the current
// VFS directory once and returns a per-open stream handle without enabling
// kernel directory caching.
func (n *Node) OpendirHandle(ctx context.Context, flags uint32) (fusefs.FileHandle, uint32, syscall.Errno) {
	vfsNode, errno := n.lookupSelf()
	if errno != 0 {
		return nil, 0, errno
	}
	dir, ok := vfsNode.(*vfs.Dir)
	if !ok {
		return nil, 0, syscall.ENOTDIR
	}

	entries, err := vfs.MapReadDir[fuse.DirEntry](dir, vfsNodeToDirEntry, 2)
	if err != nil {
		return nil, 0, translateError(err)
	}
	// go-fuse emits "." and ".." as dirents with inode values and skips their
	// readdirplus child lookup. Adding either to the real node tree triggers a
	// recovered panic, logs its stack trace, and returns EIO.
	entries[0] = fuse.DirEntry{
		Mode: fuse.S_IFDIR,
		Name: ".",
		Ino:  dir.Inode(),
	}
	entries[1] = fuse.DirEntry{
		Mode: fuse.S_IFDIR,
		Name: "..",
		Ino:  n.parentDirEntryInode(),
	}
	for i := range entries {
		entries[i].Off = uint64(i + 1)
	}

	return &mount2DirHandle{
		parent:  n,
		dir:     dir,
		entries: entries,
	}, 0, 0
}

// Readdirent returns the next entry from the per-open directory stream.
func (h *mount2DirHandle) Readdirent(ctx context.Context) (*fuse.DirEntry, syscall.Errno) {
	// Defensive EBADF; kernel orders RELEASEDIR after in-flight
	// FileReaddirenter/FileLookuper/FileSeekdirer calls on the same fh, so a
	// released handle should be unreachable, but guards against future bridge
	// changes or fork divergence.
	if h.dir == nil || h.entries == nil {
		return nil, syscall.EBADF
	}
	if h.index >= len(h.entries) {
		return nil, 0
	}
	entry := &h.entries[h.index]
	h.index++
	return entry, 0
}

// Lookup resolves a READDIRPLUS entry from the opened directory identity.
// ENOENT is valid when the entry disappears between Readdirent and Lookup.
func (h *mount2DirHandle) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fusefs.Inode, syscall.Errno) {
	// Defensive EBADF; kernel orders RELEASEDIR after in-flight
	// FileReaddirenter/FileLookuper/FileSeekdirer calls on the same fh, so a
	// released handle should be unreachable, but guards against future bridge
	// changes or fork divergence.
	if h.dir == nil || h.parent == nil {
		return nil, syscall.EBADF
	}
	if name == "" || name == "." || name == ".." {
		return nil, syscall.ENOENT
	}

	vfsNode, err := h.dir.Stat(name)
	if err != nil {
		return nil, translateError(err)
	}
	h.parent.fsys.setEntryOut(vfsNode, out)

	child := newNode(h.parent.fsys, vfsNode)
	return h.parent.EmbeddedInode().NewInode(ctx, child, fusefs.StableAttr{Mode: out.Attr.Mode, Ino: vfsNode.Inode()}), 0
}

// Releasedir drops all per-open directory state.
func (h *mount2DirHandle) Releasedir(ctx context.Context, releaseFlags uint32) {
	h.parent = nil
	h.dir = nil
	h.entries = nil
	h.index = 0
}

// Seekdir resumes after the entry whose contiguous 1-based offset the kernel
// supplies. Offsets match go-fuse's dirArray convention: index equals int(off).
func (h *mount2DirHandle) Seekdir(ctx context.Context, off uint64) syscall.Errno {
	// Defensive EBADF; kernel orders RELEASEDIR after in-flight
	// FileReaddirenter/FileLookuper/FileSeekdirer calls on the same fh, so a
	// released handle should be unreachable, but guards against future bridge
	// changes or fork divergence.
	if h.dir == nil || h.parent == nil || h.entries == nil {
		return syscall.EBADF
	}
	if off > uint64(len(h.entries)) {
		return syscall.EINVAL
	}
	if off == 0 {
		h.index = 0
		return 0
	}
	h.index = int(off)
	return 0
}
