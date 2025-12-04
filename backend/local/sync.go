// Sync support for distributed filesystems (CephFS, GlusterFS, Lustre)
//
// This file contains all logic for syncing files and directories after
// writes to ensure immediate visibility to other clients on distributed
// filesystems.
//
// Background:
// Distributed filesystems like CephFS use distributed locking. When a client
// writes data, dirty pages in the kernel buffer cache hold locks that block
// other clients from reading directory listings or file metadata. Without
// explicit fsync, these locks may be held until the dirty page writeback
// interval expires, causing other clients to hang.
//
// This implementation:
// - Syncs files immediately after write (releases locks)
// - Batches and deduplicates directory syncs for efficiency
// - Only enabled on Linux (where CephFS/GlusterFS/Lustre run)
//
// Design notes:
// - No explicit shutdown/cleanup: pending syncs may be lost on program exit.
//   This is acceptable since the worst case is other clients waiting slightly
//   longer (until dirty page writeback), and clean shutdown adds complexity
//   for minimal benefit.
// - No goroutine leaks: time.AfterFunc goroutines are short-lived and
//   self-terminating after flush() completes.
// - Root directory exclusion: Files and directories created directly under the
//   rclone root do NOT trigger parent directory syncs. This is intentional—the
//   root is typically a mount point and syncing it on every operation would be
//   wasteful. For these top-level items, visibility to other clients depends on
//   the natural dirty writeback interval.

package local

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
)

// syncWritesSupported reports whether sync writes is supported on this platform.
// Currently only Linux is supported since distributed filesystem kernel clients
// (CephFS, GlusterFS, Lustre) are Linux-specific.
func syncWritesSupported() bool {
	return runtime.GOOS == "linux"
}

// dirSyncManager batches and deduplicates directory syncs for
// distributed filesystem coherence (CephFS, GlusterFS, Lustre).
//
// When multiple files are written to the same directory, only one
// directory sync is performed per batch interval, significantly
// reducing overhead for bulk operations.
type dirSyncManager struct {
	mu       sync.Mutex
	pending  map[string]struct{}
	timer    *time.Timer
	interval time.Duration
	root     string // rclone root path - excluded from syncing
}

// newDirSyncManager creates a new directory sync manager.
// If interval <= 0, syncs are performed immediately (no batching).
// root is the rclone root path which will be excluded from syncing.
func newDirSyncManager(interval time.Duration, root string) *dirSyncManager {
	return &dirSyncManager{
		pending:  make(map[string]struct{}),
		interval: interval,
		root:     root,
	}
}

// Queue adds a directory to be synced. Requests are deduplicated
// and batched according to the configured interval.
func (m *dirSyncManager) Queue(dir string) {
	if dir == "" {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.queueLocked(dir)

	// Immediate mode flushes synchronously
	if m.interval <= 0 {
		m.flushLocked()
	}
}

// QueueMultiple queues multiple directories for syncing.
// Useful for operations that affect multiple directories (e.g., Move).
func (m *dirSyncManager) QueueMultiple(dirs ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, dir := range dirs {
		m.queueLocked(dir)
	}

	// Immediate mode flushes synchronously
	if m.interval <= 0 {
		m.flushLocked()
	}
}

// queueLocked adds a directory to pending set. Caller must hold m.mu.
func (m *dirSyncManager) queueLocked(dir string) {
	// Skip empty, filesystem root, current dir, or rclone root.
	// Root exclusion is intentional: syncing the mount point on every op is wasteful,
	// and top-level items will become visible after natural dirty writeback.
	if dir == "" || dir == "/" || dir == "." || dir == m.root {
		return
	}

	if _, exists := m.pending[dir]; exists {
		return
	}
	m.pending[dir] = struct{}{}

	// Start timer if batched mode and not already running
	if m.interval > 0 && m.timer == nil {
		m.timer = time.AfterFunc(m.interval, m.flush)
	}
}

func (m *dirSyncManager) flush() {
	m.mu.Lock()
	toSync := m.pending
	m.pending = make(map[string]struct{})
	m.timer = nil // Timer already fired; just clear reference
	m.mu.Unlock()

	// Sort directories parent-first (shorter paths first) for proper visibility
	// propagation on distributed filesystems.
	dirs := make([]string, 0, len(toSync))
	for dir := range toSync {
		dirs = append(dirs, dir)
	}
	slices.SortFunc(dirs, func(a, b string) int {
		// Sort by path depth (fewer separators = shallower = parent)
		depthA := strings.Count(a, string(filepath.Separator))
		depthB := strings.Count(b, string(filepath.Separator))
		if depthA != depthB {
			return depthA - depthB
		}
		// Stable sort by path for determinism
		return strings.Compare(a, b)
	})

	// Perform I/O outside lock to avoid blocking Queue() callers.
	for _, dir := range dirs {
		m.syncDirWithRecover(dir)
	}
}

func (m *dirSyncManager) flushLocked() {
	// Used for immediate mode (interval <= 0) where we sync synchronously.
	// Caller holds lock; we do I/O here which blocks concurrent Queue() calls.
	// This is acceptable for immediate mode since users chose responsiveness
	// over throughput.

	// Sort directories parent-first (shorter paths first) for proper visibility
	// propagation on distributed filesystems.
	dirs := make([]string, 0, len(m.pending))
	for dir := range m.pending {
		dirs = append(dirs, dir)
	}
	slices.SortFunc(dirs, func(a, b string) int {
		depthA := strings.Count(a, string(filepath.Separator))
		depthB := strings.Count(b, string(filepath.Separator))
		if depthA != depthB {
			return depthA - depthB
		}
		return strings.Compare(a, b)
	})

	for _, dir := range dirs {
		m.syncDirWithRecover(dir)
	}
	m.pending = make(map[string]struct{})
	// In immediate mode, timer is always nil (never started). This is defensive.
	m.timer = nil
}

func (m *dirSyncManager) syncDir(path string) {
	dir, err := os.Open(path)
	if err != nil {
		fs.Debugf(nil, "local: open dir for sync failed: %s: %v", path, err)
		return
	}
	if err := dir.Sync(); err != nil {
		fs.Debugf(nil, "local: dir sync failed: %s: %v", path, err)
	}
	_ = dir.Close() // Best-effort sync; close error not critical
}

// syncDirWithRecover syncs a directory with panic recovery.
// Used by flush methods to ensure all directories get attempted even if one panics.
func (m *dirSyncManager) syncDirWithRecover(path string) {
	defer func() {
		if r := recover(); r != nil {
			fs.Errorf(nil, "local: panic during dir sync %s: %v", path, r)
		}
	}()
	m.syncDir(path)
}

// syncingWriterAtCloser wraps an *os.File to add fsync on Close
// for distributed filesystem coherence. Used by OpenWriterAt for
// multi-threaded downloads.
type syncingWriterAtCloser struct {
	*os.File
	path    string
	manager *dirSyncManager
}

// Close syncs the file data and queues a directory sync before closing.
func (s *syncingWriterAtCloser) Close() error {
	// Sync file data first (releases distributed FS locks)
	syncFile(s.File, s.path)

	err := s.File.Close()

	// Queue async parent directory sync
	if s.manager != nil {
		s.manager.Queue(filepath.Dir(s.path))
	}

	return err
}

// syncFile syncs a file for distributed filesystem coherence.
// Errors are logged but not returned (best-effort for coherence).
func syncFile(f *os.File, name string) {
	if err := f.Sync(); err != nil {
		fs.Debugf(nil, "local: sync file failed: %s: %v", name, err)
	}
}
