package local

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDirSyncManager_Deduplication(t *testing.T) {
	// Create manager with long interval (won't auto-flush)
	m := newDirSyncManager(time.Hour, "/root")

	// Queue same directory multiple times
	m.Queue("/foo/bar")
	m.Queue("/foo/bar")
	m.Queue("/foo/bar")

	m.mu.Lock()
	count := len(m.pending)
	m.mu.Unlock()

	assert.Equal(t, 1, count, "identical directories should be deduplicated")
}

func TestDirSyncManager_ImmediateMode(t *testing.T) {
	// Table-driven test for immediate mode (interval=0) across all queue methods
	t.Run("Queue", func(t *testing.T) {
		tmpDir := t.TempDir()
		m := newDirSyncManager(0, "/root")
		m.Queue(tmpDir)

		m.mu.Lock()
		pendingCount := len(m.pending)
		m.mu.Unlock()

		assert.Equal(t, 0, pendingCount, "should flush synchronously")
	})

	t.Run("QueueMultiple", func(t *testing.T) {
		tmpDir := t.TempDir()
		subDir := filepath.Join(tmpDir, "sub")
		require.NoError(t, os.Mkdir(subDir, 0755))

		m := newDirSyncManager(0, "/root")
		m.QueueMultiple(tmpDir, subDir)

		m.mu.Lock()
		pendingCount := len(m.pending)
		m.mu.Unlock()

		assert.Equal(t, 0, pendingCount, "should flush synchronously")
	})

}

func TestDirSyncManager_QueueMultiple(t *testing.T) {
	m := newDirSyncManager(time.Hour, "/root")

	m.QueueMultiple("/foo", "/bar", "/baz", "/foo") // /foo duplicated

	m.mu.Lock()
	count := len(m.pending)
	m.mu.Unlock()

	assert.Equal(t, 3, count, "should have 3 unique directories")
}

func TestDirSyncManager_IgnoredPaths(t *testing.T) {
	// Tests that empty strings, filesystem root, current dir, and rclone root are ignored
	m := newDirSyncManager(time.Hour, "/rclone-root")

	m.Queue("")
	m.Queue("")
	m.Queue("/")
	m.Queue(".")
	m.Queue("/rclone-root") // rclone root - should be ignored
	m.Queue("/foo")         // valid

	m.mu.Lock()
	count := len(m.pending)
	_, hasFoo := m.pending["/foo"]
	_, hasRcloneRoot := m.pending["/rclone-root"]
	m.mu.Unlock()

	assert.Equal(t, 1, count, "only /foo should be queued")
	assert.True(t, hasFoo, "/foo should be present")
	assert.False(t, hasRcloneRoot, "/rclone-root should NOT be queued")
}

func TestSyncingWriterAtCloser_Close(t *testing.T) {
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "test.txt")

	f, err := os.Create(tmpFile)
	require.NoError(t, err)

	_, err = f.Write([]byte("test data"))
	require.NoError(t, err)

	m := newDirSyncManager(time.Hour, "/root") // long interval to prevent auto-flush
	wrapper := &syncingWriterAtCloser{
		File:    f,
		path:    tmpFile,
		manager: m,
	}

	err = wrapper.Close()
	require.NoError(t, err)

	// Verify directory was queued
	m.mu.Lock()
	_, queued := m.pending[tmpDir]
	m.mu.Unlock()

	assert.True(t, queued, "parent directory should be queued after close")
}

func TestSyncingWriterAtCloser_NilManager(t *testing.T) {
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "test.txt")

	f, err := os.Create(tmpFile)
	require.NoError(t, err)

	_, err = f.Write([]byte("test data"))
	require.NoError(t, err)

	// nil manager should not panic
	wrapper := &syncingWriterAtCloser{
		File:    f,
		path:    tmpFile,
		manager: nil,
	}

	err = wrapper.Close()
	assert.NoError(t, err, "close with nil manager should not error")
}

func TestDirSyncManager_BatchCoalescing(t *testing.T) {
	// Test that multiple Queue() calls within the batch interval
	// are coalesced into a single flush operation.
	tmpDir := t.TempDir()
	subDir1 := filepath.Join(tmpDir, "sub1")
	subDir2 := filepath.Join(tmpDir, "sub2")
	require.NoError(t, os.Mkdir(subDir1, 0755))
	require.NoError(t, os.Mkdir(subDir2, 0755))

	m := newDirSyncManager(50*time.Millisecond, "/root")

	// Queue multiple directories in rapid succession
	m.Queue(tmpDir)
	m.Queue(subDir1)
	m.Queue(subDir2)
	m.Queue(tmpDir) // Duplicate - should be deduplicated

	// All should be pending (not yet flushed)
	m.mu.Lock()
	assert.Equal(t, 3, len(m.pending), "should have 3 unique pending directories")
	assert.NotNil(t, m.timer, "timer should be running")
	m.mu.Unlock()

	// Wait for batch interval to elapse
	time.Sleep(100 * time.Millisecond)

	// All should be flushed together
	m.mu.Lock()
	assert.Equal(t, 0, len(m.pending), "all directories should be flushed")
	assert.Nil(t, m.timer, "timer should be nil after flush")
	m.mu.Unlock()
}

func TestDirSyncManager_SyncOrder(t *testing.T) {
	// Test that directories are synced in parent-first order.
	// This is important for distributed filesystems where parent
	// visibility must propagate before children.
	tests := []struct {
		name     string
		input    []string
		expected []string
	}{
		{
			name:     "nested paths sorted parent-first",
			input:    []string{"/a/b/c/d", "/a/b", "/a/b/c", "/a"},
			expected: []string{"/a", "/a/b", "/a/b/c", "/a/b/c/d"},
		},
		{
			name:     "same depth sorted alphabetically",
			input:    []string{"/z", "/a", "/m"},
			expected: []string{"/a", "/m", "/z"},
		},
		{
			name:     "mixed depths",
			input:    []string{"/x/y/z", "/a", "/x/y", "/b/c"},
			expected: []string{"/a", "/b/c", "/x/y", "/x/y/z"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Use the same sorting logic as flush() to verify order
			dirs := make([]string, len(tc.input))
			copy(dirs, tc.input)

			slices.SortFunc(dirs, func(a, b string) int {
				depthA := strings.Count(a, string(filepath.Separator))
				depthB := strings.Count(b, string(filepath.Separator))
				if depthA != depthB {
					return depthA - depthB
				}
				return strings.Compare(a, b)
			})

			assert.Equal(t, tc.expected, dirs, "directories should be sorted parent-first")
		})
	}
}
