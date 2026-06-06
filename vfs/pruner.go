package vfs

// Pruner is the callback by which a mount layer asks the kernel to FORGET the
// inodes for VFS nodes that VFS has just evicted from its own cache.
// Implementations must treat victims as read-only because the slice may be
// shared with other callbacks.
type Pruner interface {
	PruneInodes(victims []Node)
}

// prunerEntry associates an owner with a Pruner registered on the VFS.
type prunerEntry struct {
	owner  any
	pruner Pruner
}

// SetPruner registers p for owner, replacing any Pruner owner registered
// before. Registering nil removes owner's Pruner.
//
// Pruners registered by different owners are independent. Owner must be
// comparable and unique to the user - a pointer to the user's filesystem
// struct is a good choice.
func (vfs *VFS) SetPruner(owner any, p Pruner) {
	for {
		old := vfs.prunerStore.Load()
		var entries []prunerEntry
		if old != nil {
			entries = make([]prunerEntry, 0, len(*old)+1)
			for _, entry := range *old {
				if entry.owner != owner {
					entries = append(entries, entry)
				}
			}
		}
		if p != nil {
			entries = append(entries, prunerEntry{owner: owner, pruner: p})
		}
		var next *[]prunerEntry
		if len(entries) != 0 {
			next = &entries
		}
		if vfs.prunerStore.CompareAndSwap(old, next) {
			return
		}
	}
}

// pruners returns an independent snapshot of the registered Pruners.
func (vfs *VFS) pruners() []Pruner {
	entries := vfs.prunerStore.Load()
	if entries == nil {
		return nil
	}
	pruners := make([]Pruner, len(*entries))
	for i, entry := range *entries {
		pruners[i] = entry.pruner
	}
	return pruners
}

// ContentInvalidator is the callback by which a mount layer asks the kernel to
// invalidate cached content for a VFS file whose backing object changed.
type ContentInvalidator interface {
	InvalidateContent(file *File)
}

// contentInvalidatorEntry associates an owner with a ContentInvalidator
// registered on the VFS.
type contentInvalidatorEntry struct {
	owner       any
	invalidator ContentInvalidator
}

// contentInvalidatorSnapshot broadcasts an invalidation to its callbacks.
type contentInvalidatorSnapshot []ContentInvalidator

func (invalidators contentInvalidatorSnapshot) InvalidateContent(file *File) {
	for _, invalidator := range invalidators {
		invalidator.InvalidateContent(file)
	}
}

// SetContentInvalidator registers ci for owner, replacing any ContentInvalidator
// owner registered before. Registering nil removes owner's ContentInvalidator.
//
// ContentInvalidators registered by different owners are independent. Owner
// must be comparable and unique to the user - a pointer to the user's
// filesystem struct is a good choice.
func (vfs *VFS) SetContentInvalidator(owner any, ci ContentInvalidator) {
	for {
		old := vfs.contentInvalidatorStore.Load()
		var entries []contentInvalidatorEntry
		if old != nil {
			entries = make([]contentInvalidatorEntry, 0, len(*old)+1)
			for _, entry := range *old {
				if entry.owner != owner {
					entries = append(entries, entry)
				}
			}
		}
		if ci != nil {
			entries = append(entries, contentInvalidatorEntry{owner: owner, invalidator: ci})
		}
		var next *[]contentInvalidatorEntry
		if len(entries) != 0 {
			next = &entries
		}
		if vfs.contentInvalidatorStore.CompareAndSwap(old, next) {
			return
		}
	}
}

// contentInvalidators returns an independent snapshot of the registered
// ContentInvalidators.
func (vfs *VFS) contentInvalidators() contentInvalidatorSnapshot {
	entries := vfs.contentInvalidatorStore.Load()
	if entries == nil {
		return nil
	}
	invalidators := make(contentInvalidatorSnapshot, len(*entries))
	for i, entry := range *entries {
		invalidators[i] = entry.invalidator
	}
	return invalidators
}
