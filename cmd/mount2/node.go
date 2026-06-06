//go:build linux || (darwin && amd64)

package mount2

import (
	"context"
	"os"
	"path"
	"sync/atomic"
	"syscall"

	fusefs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/rclone/rclone/cmd/mountlib"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/log"
	"github.com/rclone/rclone/vfs"
)

/**
References:
	* https://github.com/octohelm/unifs/blob/main/pkg/fuse/node.go
    * https://github.com/rfjakob/gocryptfs/blob/master/internal/fusefrontend_reverse/node.go
    * https://github.com/seaweedfs/seaweedfs/tree/master/weed/mount
*/

// Node represents a directory or file
type Node struct {
	fsys *FS
	fusefs.Inode
	// vfsDir caches parent-namespace directory identity, not directory state.
	// The general rule prohibits a second long-lived vfs.Node pointer on *Node
	// because stored nodes with Path, IsDir, Open, DirEntry, or other state
	// can become stale under external mutation.
	//
	// This field is the constraint exception: it stores a *vfs.Dir identity
	// only so lookup helpers can pass that identity to dir.Stat(name). It does
	// not cache or trust leaf state. Dir.Stat(name) reads the VFS directory
	// items map, which can reuse cached statRead entries within DirCacheTime;
	// the call does not recompute strictly external updates per invocation.
	// Same-VFS mutations, including virtual entries and Dir.rename, update VFS
	// state directly; DirCacheTime bounds strictly external mutations.
	//
	// The field is consistent with the rule's intent even though *vfs.Dir
	// implements vfs.Node. It does not store child vfs.Node state, metadata,
	// open handles, or directory entries on *Node. The leaf-state staleness
	// floor under external mutation does not apply to this identity-only
	// pointer. A parent rename race can result in dir.Stat resolving against
	// the pre-rename namespace; operations resolve through the kernel parent
	// identity supplied for their inode.
	vfsDir atomic.Pointer[vfs.Dir]
}

// Node types must be InodeEmbedders
var _ fusefs.InodeEmbedder = (*Node)(nil)

// newNode returns the FUSE node attached to a VFS node, creating and attaching
// one if needed. A VFS node maps to one attached FUSE node. If the owner slot
// contains an unexpected type, newNode returns a new unattached FUSE node.
func newNode(fsys *FS, vfsNode vfs.Node) (node *Node) {
	// Check the vfsNode to see if it has a fuse Node cached
	// We must return the same fuse nodes for vfs Nodes
	if node, ok := vfsNode.Aux(fsys).(*Node); ok {
		return node
	}
	node = &Node{
		fsys: fsys,
	}
	if dir, ok := vfsNode.(*vfs.Dir); ok {
		node.vfsDir.Store(dir)
	}
	actual, _ := vfsNode.LoadOrStoreAux(fsys, node)
	if actualNode, ok := actual.(*Node); ok {
		return actualNode
	}
	fs.Errorf(vfsNode, "Unexpected auxiliary value type %T for FUSE node", actual)
	return node
}

// Path returns the path of the node relative to the root
func (n *Node) path(names ...string) string {
	return n.fsys.path(n, names...)
}

// String used for pretty printing.
func (n *Node) String() string {
	return n.path()
}

// VFS returns the VFS that this node is part of
func (n *Node) VFS() *vfs.VFS {
	return n.fsys.VFS
}

// lookup a Node given a path
func (n *Node) lookupNode(leaf string) (vfs.Node, syscall.Errno) {
	var (
		node vfs.Node
		err  error
	)
	if leaf == "" {
		node, err = n.VFS().Stat(n.path())
	} else {
		node, err = n.VFS().Stat(n.path(leaf))
	}
	return node, translateError(err)
}

// lookup a Dir given a path
func (n *Node) lookupDir(leaf string) (*vfs.Dir, syscall.Errno) {
	vfsNode, errno := n.lookupNode(leaf)
	if errno != 0 {
		return nil, errno
	}
	if !vfsNode.IsDir() {
		return nil, syscall.ENOTDIR
	}
	dir, ok := vfsNode.(*vfs.Dir)
	if !ok {
		return nil, syscall.ENOTDIR
	}
	return dir, 0
}

// lookupChild resolves name relative to n as the parent directory. It uses
// the parent directory identity cached in n.vfsDir to avoid a root-walk, then
// lets VFS Dir.Stat own freshness and synchronization for the child lookup.
//
// If the directory identity is unavailable, lookupChild falls back to the
// path-based VFS.Stat shape so callers keep the same behavior with only the
// optimization disabled for that operation.
//
// Rename races are bounded to one operation: a parent or child rename can be
// observed through the directory identity current for this call, and later
// calls resolve through the kernel's current parent identity.
func (n *Node) lookupChild(name string) (vfs.Node, syscall.Errno) {
	dir := n.vfsDir.Load()
	if dir == nil {
		return n.lookupNode(name)
	}
	vfsNode, err := dir.Stat(name)
	return vfsNode, translateError(err)
}

// lookupSelf resolves the vfs.Node represented by n from its parent's
// directory identity when possible. Root keeps the existing VFS.Stat("/")
// shape, which returns the VFS root without walking path segments.
//
// Parent recovery failures are structural fallbacks, not errors: detached
// parent windows and foreign inode operations use the path-based VFS.Stat
// resolution that the caller used before this helper.
func (n *Node) lookupSelf() (vfs.Node, syscall.Errno) {
	if n.fsys != nil && n.fsys.root == n {
		vfsNode, err := n.VFS().Stat("/")
		return vfsNode, translateError(err)
	}
	name, parentInode := n.EmbeddedInode().Parent()
	if parentInode == nil {
		return n.lookupNode("")
	}
	parent, ok := parentInode.Operations().(*Node)
	if !ok || parent == nil {
		return n.lookupNode("")
	}
	return parent.lookupChild(name)
}

// Statfs implements statistics for the filesystem that holds this
// Inode. If not defined, the `out` argument will zeroed with an OK
// result.  This is because OSX filesystems must Statfs, or the mount
// will not work.
func (n *Node) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	defer log.Trace(n, "")("out=%+v", &out)
	total, _, free := n.VFS().Statfs()
	_, ioBlockSize := n.VFS().GetBlockSizes()
	blockSize := uint64(ioBlockSize)
	out.Blocks = uint64(total) / blockSize // Total data blocks in file system.
	out.Bfree = uint64(free) / blockSize   // Free blocks in file system.
	out.Bavail = out.Bfree                 // Free blocks in file system if you're not root.
	out.Files = 1e9                        // Total files in file system.
	out.Ffree = 1e9                        // Free files in file system.
	out.Bsize = uint32(blockSize)          // Block size
	out.NameLen = 255                      // Maximum file name length?
	out.Frsize = uint32(blockSize)         // Fragment size, smallest addressable data size in the file system.
	mountlib.ClipBlocks(&out.Blocks)
	mountlib.ClipBlocks(&out.Bfree)
	mountlib.ClipBlocks(&out.Bavail)
	return 0
}

var _ = (fusefs.NodeStatfser)((*Node)(nil))

// Getattr reads attributes for an Inode. The library will ensure that
// Mode and Ino are set correctly. For files that are not opened with
// FOPEN_DIRECTIO, Size should be set so it can be read correctly.  If
// returning zeroed permissions, the default behavior is to change the
// mode of 0755 (directory) or 0644 (files). This can be switched off
// with the Options.NullPermissions setting. If blksize is unset, 4096
// is assumed, and the 'blocks' field is set accordingly.
func (n *Node) Getattr(ctx context.Context, f fusefs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	vfsNode, errno := n.lookupSelf()
	if errno != 0 {
		return errno
	}
	n.fsys.setAttrOut(vfsNode, out)
	return 0
}

var _ = (fusefs.NodeGetattrer)((*Node)(nil))

// Setattr sets attributes for an Inode.
func (n *Node) Setattr(ctx context.Context, f fusefs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) (errno syscall.Errno) {
	defer log.Trace(n, "in=%v", in)("out=%#v, errno=%v", &out, &errno)
	var err error
	vfsNode, errno := n.lookupSelf()
	if errno != 0 {
		return errno
	}
	n.fsys.setAttrOut(vfsNode, out)
	size, ok := in.GetSize()
	if ok {
		err = vfsNode.Truncate(int64(size))
		if err != nil {
			return translateError(err)
		}
		out.Attr.Size = size
	}
	mtime, ok := in.GetMTime()
	if ok {
		err = vfsNode.SetModTime(mtime)
		if err != nil {
			return translateError(err)
		}
		out.Attr.Mtime = uint64(mtime.Unix())
		out.Attr.Mtimensec = uint32(mtime.Nanosecond())
	}
	return 0
}

var _ = (fusefs.NodeSetattrer)((*Node)(nil))

// Open opens an Inode (of regular file type) for reading. It
// is optional but recommended to return a FileHandle.
func (n *Node) Open(ctx context.Context, flags uint32) (fh fusefs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	defer log.Trace(n, "flags=%#o", flags)("errno=%v", &errno)
	// fuse flags are based off syscall flags as are os flags, so
	// should be compatible
	vfsNode, errno := n.lookupSelf()
	if errno != 0 {
		return nil, 0, errno
	}
	handle, err := vfsNode.Open(int(flags))
	if err != nil {
		return nil, 0, translateError(err)
	}
	// If size unknown then use direct io to read
	if entry := vfsNode.DirEntry(); entry != nil && entry.Size() < 0 {
		fuseFlags |= fuse.FOPEN_DIRECT_IO
	}
	if n.fsys.opt.DirectIO {
		fuseFlags |= fuse.FOPEN_DIRECT_IO
	}
	return newFileHandle(handle, n.fsys), fuseFlags, 0
}

var _ = (fusefs.NodeOpener)((*Node)(nil))

// Lookup should find a direct child of a directory by the child's name.  If
// the entry does not exist, it should return ENOENT and optionally
// set a NegativeTimeout in `out`. If it does exist, it should return
// attribute data in `out` and return the Inode for the child. A new
// inode can be created using `Inode.NewInode`. The new Inode will be
// added to the FS tree automatically if the return status is OK.
//
// If a directory does not implement NodeLookuper, the library looks
// for an existing child with the given name.
//
// The input to a Lookup is {parent directory, name string}.
//
// Lookup, if successful, must return an *Inode. Once the Inode is
// returned to the kernel, the kernel can issue further operations,
// such as Open or Getxattr on that node.
//
// A successful Lookup also returns an EntryOut. Among others, this
// contains file attributes (mode, size, mtime, etc.).
//
// FUSE supports other operations that modify the namespace. For
// example, the Symlink, Create, Mknod, Link methods all create new
// children in directories. Hence, they also return *Inode and must
// populate their fuse.EntryOut arguments.
func (n *Node) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (inode *fusefs.Inode, errno syscall.Errno) {
	defer log.Trace(n, "name=%q", name)("inode=%v, attr=%v, errno=%v", &inode, &out, &errno)
	vfsNode, errno := n.lookupChild(name)
	if errno != 0 {
		return nil, errno
	}

	n.fsys.setEntryOut(vfsNode, out)

	newNode := newNode(n.fsys, vfsNode)

	return n.NewInode(ctx, newNode, fusefs.StableAttr{Mode: out.Attr.Mode, Ino: vfsNode.Inode()}), 0
}

var _ = (fusefs.NodeLookuper)((*Node)(nil))
var _ = (fusefs.NodeOpendirHandler)((*Node)(nil))

// Opendir opens a directory Inode for reading its
// contents. The actual reading is driven from Readdir, so
// this method is just for performing sanity/permission
// checks. The default is to return success.
func (n *Node) Opendir(ctx context.Context) syscall.Errno {
	_, errno := n.lookupDir("")
	if errno != 0 {
		return errno
	}
	return 0
}

var _ = (fusefs.NodeOpendirer)((*Node)(nil))

// parentDirEntryInode returns the parent inode, treating the root as its own parent.
// It returns zero when the node is detached from the bridge tree.
func (n *Node) parentDirEntryInode() uint64 {
	inode := n.EmbeddedInode()
	_, parent := inode.Parent()
	if parent != nil {
		return parent.StableAttr().Ino
	}
	if n.fsys != nil && n.fsys.root == n {
		return inode.StableAttr().Ino
	}
	return 0
}

// Readdir opens a stream of directory entries.
//
// Readdir essentially returns a list of strings, and it is allowed
// for Readdir to return different results from Lookup. For example,
// you can return nothing for Readdir ("ls my-fuse-mount" is empty),
// while still implementing Lookup ("ls my-fuse-mount/a-specific-file"
// shows a single file).
//
// If a directory does not implement NodeReaddirer, a list of
// currently known children from the tree is returned. This means that
// static in-memory file systems need not implement NodeReaddirer.
func (n *Node) Readdir(ctx context.Context) (ds fusefs.DirStream, errno syscall.Errno) {
	defer log.Trace(n, "")("ds=%v, errno=%v", &ds, &errno)
	vfsDir, errno := n.lookupDir("")
	if errno != 0 {
		return nil, errno
	}
	items, err := vfs.MapReadDir[fuse.DirEntry](vfsDir, vfsNodeToDirEntry, 2)
	if err != nil {
		return nil, translateError(err)
	}

	// go-fuse emits "." and ".." as dirents with inode values and skips their
	// readdirplus child lookup. Adding either to the real node tree triggers a
	// recovered panic, logs its stack trace, and returns EIO.
	items[0] = fuse.DirEntry{
		Mode: fuse.S_IFDIR,
		Name: ".",
		Ino:  vfsDir.Inode(),
	}
	items[1] = fuse.DirEntry{
		Mode: fuse.S_IFDIR,
		Name: "..",
		Ino:  n.parentDirEntryInode(),
	}
	// The result is unsorted because POSIX readdir does not require sorted output.
	return fusefs.NewListDirStream(items), 0
}

var _ = (fusefs.NodeReaddirer)((*Node)(nil))

// Mkdir is similar to Lookup, but must create a directory entry and Inode.
// Default is to return EROFS.
func (n *Node) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (inode *fusefs.Inode, errno syscall.Errno) {
	defer log.Trace(name, "mode=0%o", mode)("inode=%v, errno=%v", &inode, &errno)
	vfsDir, errno := n.lookupDir("")
	if errno != 0 {
		return nil, errno
	}
	newDir, err := vfsDir.Mkdir(name)
	if err != nil {
		return nil, translateError(err)
	}
	newNode := newNode(n.fsys, newDir)
	n.fsys.setEntryOut(newDir, out)
	newInode := n.NewInode(ctx, newNode, fusefs.StableAttr{Mode: out.Attr.Mode, Ino: newDir.Inode()})
	return newInode, 0
}

var _ = (fusefs.NodeMkdirer)((*Node)(nil))

// Create is similar to Lookup, but should create a new
// child. It typically also returns a FileHandle as a
// reference for future reads/writes.
// Default is to return EROFS.
func (n *Node) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (node *fusefs.Inode, fh fusefs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	defer log.Trace(n, "name=%q, flags=%#o, mode=%#o", name, flags, mode)("node=%v, fh=%v, flags=%#o, errno=%v", &node, &fh, &fuseFlags, &errno)
	vfsDir, errno := n.lookupDir("")
	if errno != 0 {
		return nil, nil, 0, errno
	}
	// translate the fuse flags to os flags
	osFlags := int(flags) | os.O_CREATE
	file, err := vfsDir.Create(name, osFlags)
	if err != nil {
		return nil, nil, 0, translateError(err)
	}
	handle, err := file.Open(osFlags)
	if err != nil {
		return nil, nil, 0, translateError(err)
	}
	fh = newFileHandle(handle, n.fsys)
	// FIXME
	// fh = &fusefs.WithFlags{
	// 	File: fh,
	// 	//FuseFlags: fuse.FOPEN_NONSEEKABLE,
	// 	OpenFlags: flags,
	// }

	// Find the created node
	vfsNode, errno := n.lookupChild(name)
	if errno != 0 {
		return nil, nil, 0, errno
	}
	n.fsys.setEntryOut(vfsNode, out)
	newNode := newNode(n.fsys, vfsNode)
	fs.Debugf(nil, "attr=%#v", out.Attr)
	newInode := n.NewInode(ctx, newNode, fusefs.StableAttr{Mode: out.Attr.Mode, Ino: vfsNode.Inode()})
	return newInode, fh, 0, 0
}

var _ = (fusefs.NodeCreater)((*Node)(nil))

// Mknod creates a regular file. The kernel NFS server creates regular files
// with MKNOD (it creates-then-opens, so vfs_create routes through fuse_create
// which sends FUSE_MKNOD when there is no open intent), so without this an
// NFS-exported mount fails every file creation with ENOTSUPP. Local and SMB
// clients create via Create (FUSE_CREATE) and are unaffected. This mirrors the
// cmd/mount (bazil) backend, which implements mknod for the same reason
// (see #2115).
//
// Device/special files are not supported by the VFS, so a non-zero rdev is
// rejected. Mknod returns no open handle (unlike Create), so the handle Create
// opens is flushed (to instantiate the file and surface any I/O error) and
// released before returning.
func (n *Node) Mknod(ctx context.Context, name string, mode uint32, rdev uint32, out *fuse.EntryOut) (node *fusefs.Inode, errno syscall.Errno) {
	defer log.Trace(n, "name=%q, mode=%#o, rdev=%d", name, mode, rdev)("node=%v, errno=%v", &node, &errno)
	if rdev != 0 {
		fs.Errorf(n, "Can't create device node %q", name)
		return nil, syscall.EIO
	}
	node, fh, _, errno := n.Create(ctx, name, uint32(os.O_CREATE|os.O_WRONLY), mode, out)
	if errno != 0 {
		return nil, errno
	}
	if fh != nil {
		if errno := fh.(fusefs.FileFlusher).Flush(ctx); errno != 0 {
			return nil, errno
		}
		_ = fh.(fusefs.FileReleaser).Release(ctx)
	}
	return node, 0
}

var _ = (fusefs.NodeMknoder)((*Node)(nil))

// Unlink should remove a child from this directory.  If the
// return status is OK, the Inode is removed as child in the
// FS tree automatically. Default is to return EROFS.
func (n *Node) Unlink(ctx context.Context, name string) (errno syscall.Errno) {
	defer log.Trace(n, "name=%q", name)("errno=%v", &errno)
	vfsNode, errno := n.lookupChild(name)
	if errno != 0 {
		return errno
	}
	return translateError(vfsNode.Remove())
}

var _ = (fusefs.NodeUnlinker)((*Node)(nil))

// Rmdir is like Unlink but for directories.
// Default is to return EROFS.
func (n *Node) Rmdir(ctx context.Context, name string) (errno syscall.Errno) {
	defer log.Trace(n, "name=%q", name)("errno=%v", &errno)
	vfsNode, errno := n.lookupChild(name)
	if errno != 0 {
		return errno
	}
	return translateError(vfsNode.Remove())
}

var _ = (fusefs.NodeRmdirer)((*Node)(nil))

// Rename should move a child from one directory to a different
// one. The change is effected in the FS tree if the return status is
// OK. Default is to return EROFS.
func (n *Node) Rename(ctx context.Context, oldName string, newParent fusefs.InodeEmbedder, newName string, flags uint32) (errno syscall.Errno) {
	defer log.Trace(n, "oldName=%q, newParent=%v, newName=%q", oldName, newParent, newName)("errno=%v", &errno)
	vfsDir, errno := n.lookupDir("")
	if errno != 0 {
		return errno
	}
	newParentNode, ok := newParent.(*Node)
	if !ok {
		fs.Errorf(n, "newParent was not a *Node")
		return syscall.EIO
	}
	newDir, errno := newParentNode.lookupDir("")
	if errno != 0 {
		return syscall.ENOTDIR
	}
	return translateError(vfsDir.Rename(oldName, newName, newDir))
}

var _ = (fusefs.NodeRenamer)((*Node)(nil))

// Getxattr should read data for the given attribute into
// `dest` and return the number of bytes. If `dest` is too
// small, it should return ERANGE and the size of the attribute.
// If not defined, Getxattr will return ENOATTR.
func (n *Node) Getxattr(ctx context.Context, attr string, dest []byte) (uint32, syscall.Errno) {
	return 0, syscall.ENOSYS // we never implement this
}

var _ fusefs.NodeGetxattrer = (*Node)(nil)

// Setxattr should store data for the given attribute.  See
// setxattr(2) for information about flags.
// If not defined, Setxattr will return ENOATTR.
func (n *Node) Setxattr(ctx context.Context, attr string, data []byte, flags uint32) syscall.Errno {
	return syscall.ENOSYS // we never implement this
}

var _ fusefs.NodeSetxattrer = (*Node)(nil)

// Removexattr should delete the given attribute.
// If not defined, Removexattr will return ENOATTR.
func (n *Node) Removexattr(ctx context.Context, attr string) syscall.Errno {
	return syscall.ENOSYS // we never implement this
}

var _ fusefs.NodeRemovexattrer = (*Node)(nil)

// Listxattr should read all attributes (null terminated) into
// `dest`. If the `dest` buffer is too small, it should return ERANGE
// and the correct size.  If not defined, return an empty list and
// success.
func (n *Node) Listxattr(ctx context.Context, dest []byte) (uint32, syscall.Errno) {
	return 0, syscall.ENOSYS // we never implement this
}

var _ fusefs.NodeListxattrer = (*Node)(nil)

var _ fusefs.NodeReadlinker = (*Node)(nil)

// Readlink read symbolic link target.
func (n *Node) Readlink(ctx context.Context) (ret []byte, err syscall.Errno) {
	defer log.Trace(n, "")("ret=%v, err=%v", &ret, &err)
	nodePath := n.path()
	s, serr := n.VFS().Readlink(nodePath)
	return []byte(s), translateError(serr)
}

var _ fusefs.NodeSymlinker = (*Node)(nil)

// Symlink create symbolic link.
func (n *Node) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (node *fusefs.Inode, err syscall.Errno) {
	defer log.Trace(n, "name=%v, target=%v", name, target)("node=%v, err=%v", &node, &err)
	fullPath := path.Join(n.path(), name)
	vfsNode, serr := n.VFS().CreateSymlink(target, fullPath)
	if serr != nil {
		return nil, translateError(serr)
	}

	n.fsys.setEntryOut(vfsNode, out)
	newNode := newNode(n.fsys, vfsNode)
	newInode := n.NewInode(ctx, newNode, fusefs.StableAttr{Mode: out.Attr.Mode, Ino: vfsNode.Inode()})

	return newInode, 0
}
