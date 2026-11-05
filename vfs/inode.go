package vfs

import (
	"sync/atomic"

	"github.com/rclone/rclone/fs"
)

const (
	inodeKindDir  = 'd'
	inodeKindFile = 'f'
)

// createdInodeCount supplies process-unique identities for files and
// directories created through the VFS until a listing rebuilds them.
var createdInodeCount atomic.Uint64

// inodeHash is a running FNV-1a state over the bytes of a VFS path.
type inodeHash uint64

func (h inodeHash) mix(b byte) inodeHash {
	h ^= inodeHash(b)
	return h * 1099511628211
}

func (h inodeHash) mixString(s string) inodeHash {
	for i := 0; i < len(s); i++ {
		h = h.mix(s[i])
	}
	return h
}

// inodePrefix returns the state after directory path dPath and, unless
// dPath is the root, a separating '/'.
func inodePrefix(dPath string) inodeHash {
	h := inodeHash(14695981039346656037).mixString(dPath)
	if dPath != "" {
		h = h.mix('/')
	}
	return h
}

// inode returns the inode number of entry leaf below the prefix, of
// kind, with backend or temporary identity id (0 if unknown).
//
// The result is a compatibility contract: it must not change between
// releases, because clients keep (st_dev, st_ino) across restarts.
func (h inodeHash) inode(leaf string, kind byte, id uint64) uint64 {
	h = h.mixString(leaf).mix(kind)
	for i := 0; i < 8; i++ {
		h = h.mix(byte(id >> (8 * i)))
	}
	return clampInode(fmix64(uint64(h)))
}

func fmix64(h uint64) uint64 {
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	h *= 0xc4ceb9fe1a85ec53
	h ^= h >> 33
	return h
}

func clampInode(h uint64) uint64 {
	h &= 1<<63 - 1
	switch h {
	case 0, 1:
		h += 2
	case 1<<63 - 1:
		h--
	}
	return h
}

func (vfs *VFS) entryIdentity(e any) uint64 {
	if vfs.Opt.NoEntryIdentity {
		return 0
	}
	if x, ok := e.(fs.EntryIdentifier); ok {
		return x.EntryIdentity()
	}
	return 0
}

func (vfs *VFS) dirInode(entry fs.Directory) uint64 {
	return inodePrefix("").inode(entry.Remote(), inodeKindDir, vfs.entryIdentity(entry))
}
