//go:build linux || (darwin && amd64)

package mount2

import (
	"context"
	"sync"
	"syscall"

	fusefs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/rclone/rclone/fs"
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
	caching bool // opened with FOPEN_CACHE_DIR; counted in parent.dir.caching
	run     bool // reading a run from offset 0 to end of directory
}

// dirKey identifies the listing the kernel directory cache may hold.
type dirKey struct {
	generation uint64 // vfs.Dir listing generation; 0 means not set
	parentIno  uint64 // inode ".." lists
}

// dirState is the kernel directory cache state of a directory Node.
type dirState struct {
	mu      sync.Mutex      // protects the following
	key     dirKey          // listing the kernel cache may hold or is being filled from
	caching int             // open handles returned with FOPEN_CACHE_DIR
	snap    []fuse.DirEntry // shared entries for key; nil until captured
	runDone bool            // a caching handle of key read from offset 0 to the end
	refills int             // runs from offset 0 after runDone
	warned  bool            // the stranded-fill line was logged
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

// vfsLeafToDirEntry converts a VFS node to a FUSE directory entry named by its
// published leaf, which differs from n.Name() while a rename is running or
// after one failed.
func vfsLeafToDirEntry(leaf string, n vfs.Node) (fuse.DirEntry, error) {
	return fuse.DirEntry{
		Mode: getMode(n),
		Name: leaf,
		Ino:  n.Inode(),
	}, nil
}

// kernelDirCacheFlags tell the kernel to read and fill its directory cache.
const kernelDirCacheFlags = fuse.FOPEN_CACHE_DIR | fuse.FOPEN_KEEP_CACHE

// OpendirHandle implements fusefs.NodeOpendirHandler. It resolves the current
// VFS directory once and returns a per-open stream handle. With the kernel
// directory cache on, it returns the flags that let the kernel reuse an
// unchanged listing.
func (n *Node) OpendirHandle(ctx context.Context, flags uint32) (fusefs.FileHandle, uint32, syscall.Errno) {
	vfsNode, errno := n.lookupSelf()
	if errno != 0 {
		return nil, 0, errno
	}
	dir, ok := vfsNode.(*vfs.Dir)
	if !ok {
		return nil, 0, syscall.ENOTDIR
	}

	if n.fsys.dirCache {
		return n.opendirCached(dir)
	}

	entries, err := vfs.MapReadDir[fuse.DirEntry](dir, vfsNodeToDirEntry, 2)
	if err != nil {
		return nil, 0, translateError(err)
	}
	n.fillDotEntries(dir, entries)

	return &mount2DirHandle{
		parent:  n,
		dir:     dir,
		entries: entries,
	}, 0, 0
}

// fillDotEntries sets "." and ".." in entries[0:2] and the contiguous 1-based
// offsets of all entries.
func (n *Node) fillDotEntries(dir *vfs.Dir, entries []fuse.DirEntry) {
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
}

// privateEntries lists dir for a handle that bypasses the shared snapshot. It
// names entries by their published leaf, as the snapshot does.
func (n *Node) privateEntries(dir *vfs.Dir) ([]fuse.DirEntry, syscall.Errno) {
	entries, _, err := vfs.MapReadDirGeneration(dir, vfsLeafToDirEntry, 2)
	if err != nil {
		return nil, translateError(err)
	}
	n.fillDotEntries(dir, entries)
	return entries, 0
}

// opendirCached opens dir with the kernel directory cache on.
//
// The kernel keeps the pages of one listing per directory inode. A listing is
// identified by dirKey. Handles that fill the cache share one snapshot, so
// they all emit the same names at the same offsets. The first open that sees a
// new key with no caching handle open drops the pages with NotifyContent
// before it replies. An open that sees a new key while a caching handle is
// open gets FOPEN_KEEP_CACHE alone: it leaves the pages in place and lists
// privately.
//
// Lock order: go-fuse file mutex, dirState.mu, vfs.Dir.mu.
func (n *Node) opendirCached(dir *vfs.Dir) (fusefs.FileHandle, uint32, syscall.Errno) {
	generation, err := dir.Generation()
	if err != nil {
		return nil, 0, translateError(err)
	}
	key := dirKey{generation: generation, parentIno: n.parentDirEntryInode()}
	state := n.dir

	state.mu.Lock()
	switch {
	case state.key.generation == 0:
		// No caching handle of this Node has filled pages.
		state.key = key
	case key != state.key && state.caching > 0:
		state.mu.Unlock()
		return n.opendirPrivate(dir)
	case key != state.key:
		// ENOENT means the kernel holds no inode, so it holds no pages.
		if errno := n.EmbeddedInode().NotifyContent(0, 0); errno != 0 && errno != syscall.ENOENT {
			state.mu.Unlock()
			return n.opendirPrivate(dir)
		}
		state.key = key
		state.snap = nil
		state.runDone = false
		state.refills = 0
	}
	if state.snap == nil {
		snap, g2, err := vfs.MapReadDirGeneration(dir, vfsLeafToDirEntry, 2)
		if err != nil {
			state.mu.Unlock()
			return nil, 0, translateError(err)
		}
		n.fillDotEntries(dir, snap)
		if g2 != key.generation {
			// The listing changed after Generation. No caching handle exists,
			// so the pages of the new listing start empty.
			state.key = dirKey{generation: g2, parentIno: key.parentIno}
		}
		state.snap = snap
	}
	state.caching++
	snap := state.snap
	state.mu.Unlock()

	return &mount2DirHandle{
		parent:  n,
		dir:     dir,
		entries: snap,
		caching: true,
	}, kernelDirCacheFlags, 0
}

// opendirPrivate returns a handle over private entries that keeps the kernel
// pages and does not read or fill them.
func (n *Node) opendirPrivate(dir *vfs.Dir) (fusefs.FileHandle, uint32, syscall.Errno) {
	entries, errno := n.privateEntries(dir)
	if errno != 0 {
		return nil, 0, errno
	}
	return &mount2DirHandle{
		parent:  n,
		dir:     dir,
		entries: entries,
	}, fuse.FOPEN_KEEP_CACHE, 0
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
	if h.caching {
		h.detectStrandedFill()
	}
	if h.index >= len(h.entries) {
		return nil, 0
	}
	entry := &h.entries[h.index]
	h.index++
	return entry, 0
}

// detectStrandedFill tracks runs from offset 0 to the end of the directory on
// a caching handle. Once a run has completed with no page drop, the kernel
// cache holds the whole listing, so another run from offset 0 under the same
// key means the kernel did not keep it. Three of them log once per Node.
func (h *mount2DirHandle) detectStrandedFill() {
	// Lock only where state changes; go-fuse serializes calls on a handle.
	if h.index != 0 && !(h.index >= len(h.entries) && h.run) {
		return
	}
	state := h.parent.dir
	warn := false
	state.mu.Lock()
	switch {
	case h.index == 0:
		h.run = true
		if state.runDone {
			state.refills++
			if state.refills == 3 && !state.warned {
				state.warned = true
				warn = true
			}
		}
	case h.index >= len(h.entries) && h.run:
		state.runDone = true
	}
	state.mu.Unlock()
	if warn {
		fs.Infof(nil, "kernel directory cache for %q is not completing; reads stay uncached until the directory is evicted", h.parent.path())
	}
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
	return h.parent.EmbeddedInode().NewInode(ctx, child, child.stableAttr(out.Attr.Mode, vfsNode.Inode())), 0
}

// Releasedir drops all per-open directory state.
func (h *mount2DirHandle) Releasedir(ctx context.Context, releaseFlags uint32) {
	// The shared snapshot stays with the Node until the listing changes.
	if h.caching && h.parent != nil {
		h.parent.dir.mu.Lock()
		h.parent.dir.caching--
		h.parent.dir.mu.Unlock()
	}
	h.caching = false
	h.run = false
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
	// A run that starts past offset 0 does not complete the kernel cache.
	h.run = false
	h.index = int(off)
	return 0
}
