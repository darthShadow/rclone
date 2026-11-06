package vfscache

import (
	"context"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dMakeDirty creates a dirty item called name holding data
func dMakeDirty(t *testing.T, c *Cache, name, data string) *Item {
	item := c.Item(name)
	require.NoError(t, item.Open(nil))
	_, err := item.WriteAt([]byte(data), 0)
	require.NoError(t, err)
	require.True(t, item.IsDirty())
	return item
}

// TestDirtyItemCleanPathTakesNoCacheMutex checks that looking up a
// clean name does not need c.mu, even while another item is dirty.
func TestDirtyItemCleanPathTakesNoCacheMutex(t *testing.T) {
	_, c := newTestCache(t)

	potato := dMakeDirty(t, c, "potato", "hello")
	_ = c.Item("sub/clean")

	c.mu.Lock()
	unlocked := false
	unlock := func() {
		if !unlocked {
			unlocked = true
			c.mu.Unlock()
		}
	}
	defer unlock()

	done := make(chan []*Item, 1)
	go func() {
		done <- []*Item{
			c.DirtyItem("sub/clean"),
			c.DirtyItemAt("sub", "clean"),
			c.DirtyItem("absent"),
			c.DirtyItemAt("", "absent"),
		}
	}()
	select {
	case got := <-done:
		for _, item := range got {
			assert.Nil(t, item)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("clean-path lookup blocked on the cache mutex")
	}
	unlock()
	assert.Equal(t, potato, c.DirtyItemAt("", "potato"))
}

// TestDirtyItemAtCleanAllocs checks the clean-path lookup allocates nothing.
func TestDirtyItemAtCleanAllocs(t *testing.T) {
	_, c := newTestCache(t)

	dMakeDirty(t, c, "potato", "hello")
	_ = c.Item("sub/clean")

	allocs := testing.AllocsPerRun(200, func() {
		_ = c.DirtyItemAt("sub", "clean")
	})
	assert.Equal(t, 0.0, allocs)
}

// TestDirtyItemTracksState checks the lookup follows the dirty state
// and the key of the item through every event which changes either.
func TestDirtyItemTracksState(t *testing.T) {
	t.Run("Dirty", func(t *testing.T) {
		_, c := newTestCache(t)
		item := c.Item("a/b")
		assert.Nil(t, c.DirtyItemAt("a", "b"))
		require.NoError(t, item.Open(nil))
		assert.Nil(t, c.DirtyItemAt("a", "b"))
		_, err := item.WriteAt([]byte("12345"), 0)
		require.NoError(t, err)
		assert.Equal(t, item, c.DirtyItemAt("a", "b"))
		assert.Equal(t, item, c.DirtyItem("a/b"))
		size, err := c.DirtyItemAt("a", "b").GetSize()
		require.NoError(t, err)
		assert.Equal(t, int64(5), size)
	})
	t.Run("Close", func(t *testing.T) {
		_, c := newTestCache(t)
		item := dMakeDirty(t, c, "a/b", "12345")
		require.NoError(t, item.Close(nil))
		assert.False(t, item.IsDirty())
		assert.Nil(t, c.DirtyItemAt("a", "b"))
	})
	t.Run("Remove", func(t *testing.T) {
		_, c := newTestCache(t)
		dMakeDirty(t, c, "a/b", "12345")
		c.Remove("a/b")
		assert.Nil(t, c.DirtyItemAt("a", "b"))
		assert.Nil(t, c.DirtyItem("a/b"))
	})
	t.Run("Rename", func(t *testing.T) {
		_, c := newTestCache(t)
		item := dMakeDirty(t, c, "a/b", "12345")
		require.NoError(t, c.Rename("a/b", "c/d", nil))
		assert.Nil(t, c.DirtyItemAt("a", "b"))
		assert.Equal(t, item, c.DirtyItemAt("c", "d"))
		require.NoError(t, c.Rename("c/d", "top", nil))
		assert.Nil(t, c.DirtyItemAt("c", "d"))
		assert.Equal(t, item, c.DirtyItemAt("", "top"))
	})
	t.Run("RenameOverClean", func(t *testing.T) {
		_, c := newTestCache(t)
		dirty := dMakeDirty(t, c, "a/dirty", "12345")
		other := c.Item("a/other")
		require.NoError(t, other.Open(nil))
		require.NoError(t, other.Close(nil))
		require.NoError(t, c.Rename("a/other", "a/dirty", nil))
		assert.NotEqual(t, dirty, c.DirtyItemAt("a", "dirty"))
		assert.Nil(t, c.DirtyItemAt("a", "dirty"))
	})
	t.Run("DirRename", func(t *testing.T) {
		_, c := newTestCache(t)
		item := dMakeDirty(t, c, "dir/sub/file", "12345")
		require.NoError(t, c.DirRename("dir", "dir2"))
		assert.Nil(t, c.DirtyItemAt("dir/sub", "file"))
		assert.Equal(t, item, c.DirtyItemAt("dir2/sub", "file"))
	})
	t.Run("Truncate", func(t *testing.T) {
		_, c := newTestCache(t)
		item := dMakeDirty(t, c, "a/b", "12345")
		require.NoError(t, item.Truncate(2))
		got := c.DirtyItemAt("a", "b")
		require.NotNil(t, got)
		size, err := got.GetSize()
		require.NoError(t, err)
		assert.Equal(t, int64(2), size)
	})
	t.Run("Reload", func(t *testing.T) {
		r, c := newTestCache(t)
		dMakeDirty(t, c, "a/b", "12345")

		// Start a second cache over the same directories while the
		// first item is still dirty on disk. Keep the upload queued so
		// the reloaded item stays dirty.
		opt := vfscommon.Opt
		opt.CachePollInterval = 0
		opt.WriteBack = fs.Duration(time.Hour)
		opt.HandleCaching = 0
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		c2, err := New(ctx, r.Fremote, &opt, addVirtual)
		require.NoError(t, err)

		got := c2.DirtyItemAt("a", "b")
		require.NotNil(t, got)
		size, err := got.GetSize()
		require.NoError(t, err)
		assert.Equal(t, int64(5), size)
		assert.Nil(t, c2.DirtyItemAt("a", "other"))
	})
}

// TestDirtyItemDetached checks an item the cache no longer holds under
// its name stays out of the dirty index, even when written through a
// handle that is still open.
func TestDirtyItemDetached(t *testing.T) {
	// dOpenClean returns a clean item called name which is open
	dOpenClean := func(t *testing.T, c *Cache, name string) *Item {
		item := c.Item(name)
		require.NoError(t, item.Open(nil))
		require.False(t, item.IsDirty())
		return item
	}
	dWrite := func(t *testing.T, item *Item, data string) {
		_, err := item.WriteAt([]byte(data), 0)
		require.NoError(t, err)
		require.True(t, item.IsDirty())
	}
	t.Run("RemoveThenWrite", func(t *testing.T) {
		_, c := newTestCache(t)
		old := dOpenClean(t, c, "a/b")
		c.Remove("a/b")
		dWrite(t, old, "12345")
		assert.Nil(t, c.DirtyItemAt("a", "b"))
		assert.Nil(t, c.DirtyItem("a/b"))
	})
	t.Run("RemoveRecreatedThenWrite", func(t *testing.T) {
		_, c := newTestCache(t)
		old := dOpenClean(t, c, "a/b")
		c.Remove("a/b")
		recreated := dMakeDirty(t, c, "a/b", "new")
		require.NotSame(t, old, recreated)
		dWrite(t, old, "12345")
		assert.Same(t, recreated, c.DirtyItemAt("a", "b"))
	})
	t.Run("RemoveRecreatedCleanThenWrite", func(t *testing.T) {
		_, c := newTestCache(t)
		old := dOpenClean(t, c, "a/b")
		c.Remove("a/b")
		_ = dOpenClean(t, c, "a/b")
		dWrite(t, old, "12345")
		assert.Nil(t, c.DirtyItemAt("a", "b"))
	})
	t.Run("RenameOverThenWrite", func(t *testing.T) {
		_, c := newTestCache(t)
		dirty := dMakeDirty(t, c, "a/dirty", "12345")
		replaced := dOpenClean(t, c, "a/other")
		require.NoError(t, c.Rename("a/dirty", "a/other", nil))
		dWrite(t, replaced, "abc")
		assert.Same(t, dirty, c.DirtyItemAt("a", "other"))
	})
	t.Run("PutDisplacesAndRestores", func(t *testing.T) {
		_, c := newTestCache(t)
		first := dMakeDirty(t, c, "a/b", "12345")
		// A clean item which has never been written under the name.
		second := newItem(c, "a/other")
		second.name = "a/b"
		require.Same(t, first, c.put("a/b", second))
		assert.Nil(t, c.DirtyItemAt("a", "b"))
		require.Same(t, second, c.put("a/b", first))
		assert.Same(t, first, c.DirtyItemAt("a", "b"))
	})
}

// TestDirtyItemRenameWhileDirtied checks a rename keeps the index
// entry of an item which turned dirty after the rename set its name
// but before the index followed.
func TestDirtyItemRenameWhileDirtied(t *testing.T) {
	_, c := newTestCache(t)
	item := c.Item("a/b")
	require.NoError(t, item.Open(nil))
	require.NoError(t, item.rename("a/b", "c/d", nil))

	// Hold the index so moveDirty is stuck when the item turns dirty.
	c.dirtyMu.Lock()
	moved := make(chan struct{})
	go func() {
		defer close(moved)
		c.mu.Lock()
		defer c.mu.Unlock()
		c.moveDirty("a/b", "c/d", item)
	}()
	time.Sleep(100 * time.Millisecond)
	item.mu.Lock()
	item.info.Dirty = true
	item.mu.Unlock()
	c.dirty[splitName("c/d")] = item
	c.dirtyMu.Unlock()
	<-moved

	assert.Same(t, item, c.DirtyItemAt("c", "d"))
}

// TestDirtyItemAlreadyOpen checks that opening a second item under the
// name of an open item puts the open item back, keeps it indexed, and
// leaves the opener detached.
func TestDirtyItemAlreadyOpen(t *testing.T) {
	_, c := newTestCache(t)
	a := dMakeDirty(t, c, "x", "hello")
	b := newItem(c, "x")
	err := b.Open(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already open")
	c.mu.Lock()
	cur := c.item["x"]
	c.mu.Unlock()
	assert.Same(t, a, cur)
	assert.Same(t, a, c.DirtyItemAt("", "x"))
	a.mu.Lock()
	assert.False(t, a.detached)
	a.mu.Unlock()
	b.mu.Lock()
	assert.True(t, b.detached)
	b.mu.Unlock()
}
