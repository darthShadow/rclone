// FUSE main Fs

//go:build linux || (darwin && amd64)

package mount2

import (
	"os"
	"syscall"
	"time"

	fusefs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/rclone/rclone/cmd/mountlib"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/log"
	"github.com/rclone/rclone/lib/join"
	"github.com/rclone/rclone/vfs"
)

// FS represents the top level filing system
type FS struct {
	VFS  *vfs.VFS
	f    fs.Fs
	opt  *mountlib.Options
	root *Node
}

// NewFS creates a pathfs.FileSystem from the fs.Fs passed in
func NewFS(VFS *vfs.VFS, opt *mountlib.Options) *FS {
	fsys := &FS{
		VFS: VFS,
		f:   VFS.Fs(),
		opt: opt,
	}
	return fsys
}

func (f *FS) path(node *Node, names ...string) string {
	// Check if the node is the root node
	if f.root == node {
		if len(names) == 0 {
			return "/"
		}
		return join.FilePathJoin(append([]string{""}, names...)...)
	}

	rootNode := node.EmbeddedInode().Root()
	nodePath := node.Path(rootNode)
	nodePathElements := []string{"", nodePath}
	if len(names) == 0 {
		return join.FilePathJoin(nodePathElements...)
	}
	return join.FilePathJoin(append(nodePathElements, names...)...)
}

// Root returns the root node
func (f *FS) Root() (node *Node, err error) {
	defer log.Trace("", "")("node=%+v, err=%v", &node, &err)
	root, err := f.VFS.Root()
	if err != nil {
		return nil, err
	}
	f.root = newNode(f, root)
	return f.root, nil
}

func (f *FS) nodeFor(vfsNode vfs.Node) *Node {
	node, _ := vfsNode.Aux(f).(*Node)
	return node
}

// pruneCandidates converts VFS victims into this mount's inodes.
func (f *FS) pruneCandidates(victims []vfs.Node) []*fusefs.Inode {
	if len(victims) == 0 || f.root == nil {
		return nil
	}
	rootInode := f.root.EmbeddedInode()
	if rootInode == nil {
		return nil
	}

	seen := make(map[*fusefs.Inode]struct{}, len(victims))
	inodes := make([]*fusefs.Inode, 0, len(victims))
	for _, victim := range victims {
		node := f.nodeFor(victim)
		if node == nil {
			continue
		}
		inode := node.EmbeddedInode()
		if inode == nil {
			continue
		}
		// ForgetAll provides children only; keep pointer identity as the root
		// guard because StableAttr.Ino is not the FUSE root nodeId.
		if inode == rootInode {
			continue
		}
		if _, parent := inode.Parent(); parent == nil {
			continue // never entered this bridge's tree, so it has no nodeId
		}
		if _, ok := seen[inode]; ok {
			continue
		}
		seen[inode] = struct{}{}
		inodes = append(inodes, inode)
	}
	return inodes
}

// PruneInodes converts VFS victims into go-fuse inodes and issues a
// best-effort prune notification for defensively valid non-root nodes.
func (f *FS) PruneInodes(victims []vfs.Node) {
	inodes := f.pruneCandidates(victims)
	if len(inodes) == 0 {
		return
	}

	if errno := f.root.EmbeddedInode().NotifyPrune(inodes); errno != 0 && errno != syscall.ENOSYS {
		fs.Debugf(f.f, "NotifyPrune: %d victims, errno=%v", len(inodes), errno)
	}
}

// SetDebug if called, provide debug output through the log package.
func (f *FS) SetDebug(debug bool) {
	fs.Debugf(f.f, "SetDebug %v", debug)
}

// get the Mode from a vfs Node
func getMode(node os.FileInfo) uint32 {
	vfsMode := node.Mode()
	Mode := vfsMode.Perm()
	if vfsMode&os.ModeDir != 0 {
		Mode |= fuse.S_IFDIR
	} else if vfsMode&os.ModeSymlink != 0 {
		Mode |= fuse.S_IFLNK
	} else if vfsMode&os.ModeNamedPipe != 0 {
		Mode |= fuse.S_IFIFO
	} else {
		Mode |= fuse.S_IFREG
	}
	return uint32(Mode)
}

// fill in attr from node
func (f *FS) setAttr(node vfs.Node, attr *fuse.Attr) {
	size := uint64(node.Size())
	vfs := node.VFS()
	dataBlockSize, ioBlockSize := vfs.GetBlockSizes()
	blocks := (size + uint64(dataBlockSize) - 1) / uint64(dataBlockSize)
	modTime := node.ModTime()
	// set attributes
	attr.Owner.Gid = vfs.Opt.GID
	attr.Owner.Uid = vfs.Opt.UID
	attr.Ino = node.Inode()
	attr.Mode = getMode(node)
	attr.Size = size
	attr.Nlink = 1
	attr.Blocks = blocks
	attr.Blksize = uint32(ioBlockSize)
	s := uint64(modTime.Unix())
	ns := uint32(modTime.Nanosecond())
	attr.Atime = s
	attr.Atimensec = ns
	attr.Mtime = s
	attr.Mtimensec = ns
	attr.Ctime = s
	attr.Ctimensec = ns
	//attr.Rdev
}

// fill in AttrOut from node
func (f *FS) setAttrOut(node vfs.Node, out *fuse.AttrOut) {
	f.setAttr(node, &out.Attr)
	out.SetTimeout(time.Duration(f.opt.AttrTimeout))
}

// fill in EntryOut from node
func (f *FS) setEntryOut(node vfs.Node, out *fuse.EntryOut) {
	f.setAttr(node, &out.Attr)
	out.SetEntryTimeout(time.Duration(f.opt.AttrTimeout))
	out.SetAttrTimeout(time.Duration(f.opt.AttrTimeout))
}

// Translate errors from mountlib into Syscall error numbers
func translateError(err error) syscall.Errno {
	if err == nil {
		return 0
	}
	_, uErr := fserrors.Cause(err)
	switch uErr {
	case vfs.OK:
		return 0
	case vfs.ENOENT, fs.ErrorDirNotFound, fs.ErrorObjectNotFound:
		return syscall.ENOENT
	case vfs.EEXIST, fs.ErrorDirExists:
		return syscall.EEXIST
	case vfs.EPERM, fs.ErrorPermissionDenied:
		return syscall.EPERM
	case vfs.ECLOSED:
		return syscall.EBADF
	case vfs.ENOTEMPTY:
		return syscall.ENOTEMPTY
	case vfs.ESPIPE:
		return syscall.ESPIPE
	case vfs.EBADF:
		return syscall.EBADF
	case vfs.EROFS:
		return syscall.EROFS
	case vfs.ENOSYS, fs.ErrorNotImplemented:
		return syscall.ENOSYS
	case vfs.EINVAL:
		return syscall.EINVAL
	case vfs.ELOOP:
		return syscall.ELOOP
	}
	fs.Errorf(nil, "IO error: %v", err)
	return syscall.EIO
}
