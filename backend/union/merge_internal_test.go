package union

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"weak"

	"github.com/rclone/rclone/backend/union/policy"
	"github.com/rclone/rclone/backend/union/upstream"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fstest/mockdir"
	"github.com/rclone/rclone/fstest/mockobject"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newMergeTestFs makes a union Fs with just what mergeDirEntries needs
func newMergeTestFs(t testing.TB, caseInsensitive bool) *Fs {
	p, err := policy.Get("epff")
	require.NoError(t, err)
	return &Fs{
		features:     &fs.Features{CaseInsensitive: caseInsensitive},
		searchPolicy: p,
	}
}

// objectEntries wraps a mock object for each name as an entry of u
func objectEntries(u *upstream.Fs, names ...string) []upstream.Entry {
	entries := make([]upstream.Entry, len(names))
	for i, name := range names {
		entries[i] = u.WrapObject(mockobject.New(name))
	}
	return entries
}

// candidates returns the upstream entries behind a merged union entry
func candidates(t testing.TB, e fs.DirEntry) []upstream.Entry {
	switch e := e.(type) {
	case *Object:
		return e.co
	case *Directory:
		return e.cd
	}
	require.Failf(t, "unexpected entry type", "%T", e)
	return nil
}

// mergedByName merges the lists and returns the candidates keyed by the
// remote of the union entry
func mergedByName(t testing.TB, f *Fs, lists [][]upstream.Entry) map[string][]upstream.Entry {
	merged, err := f.mergeDirEntries(lists)
	require.NoError(t, err)
	byName := make(map[string][]upstream.Entry, len(merged))
	for _, e := range merged {
		_, dup := byName[e.Remote()]
		require.False(t, dup, "duplicate entry %q", e.Remote())
		byName[e.Remote()] = candidates(t, e)
	}
	return byName
}

func TestMergeDirEntries(t *testing.T) {
	u0, u1, u2 := &upstream.Fs{}, &upstream.Fs{}, &upstream.Fs{}
	f := newMergeTestFs(t, false)
	lists := [][]upstream.Entry{
		objectEntries(u0, "a", "b", "c"),
		nil,
		objectEntries(u1, "b", "d", "e"),
		append(objectEntries(u2, "c", "b", "f"), u2.WrapDirectory(mockdir.New("dir"))),
	}
	byName := mergedByName(t, f, lists)

	ufs := func(name string) (out []*upstream.Fs) {
		for _, e := range byName[name] {
			out = append(out, e.UpstreamFs())
		}
		return out
	}
	assert.Len(t, byName, 7)
	assert.Equal(t, []*upstream.Fs{u0}, ufs("a"))
	assert.Equal(t, []*upstream.Fs{u0, u1, u2}, ufs("b"), "grouped in upstream order")
	assert.Equal(t, []*upstream.Fs{u0, u2}, ufs("c"))
	assert.Equal(t, []*upstream.Fs{u1}, ufs("d"))
	assert.Equal(t, []*upstream.Fs{u1}, ufs("e"))
	assert.Equal(t, []*upstream.Fs{u2}, ufs("f"))
	assert.Equal(t, []*upstream.Fs{u2}, ufs("dir"))
	for name, es := range byName {
		for _, e := range es {
			assert.Equal(t, name, e.Remote())
		}
	}

	t.Run("DirectoryCandidates", func(t *testing.T) {
		merged, err := f.mergeDirEntries([][]upstream.Entry{
			{u0.WrapDirectory(mockdir.New("d"))},
			{u1.WrapDirectory(mockdir.New("d"))},
		})
		require.NoError(t, err)
		require.Len(t, merged, 1)
		d, ok := merged[0].(*Directory)
		require.True(t, ok, "got %T", merged[0])
		require.Len(t, d.cd, 2)
		assert.Same(t, u0, d.cd[0].UpstreamFs())
		assert.Same(t, u1, d.cd[1].UpstreamFs())
	})

	t.Run("NoLists", func(t *testing.T) {
		merged, err := f.mergeDirEntries([][]upstream.Entry{nil, nil})
		require.NoError(t, err)
		assert.Empty(t, merged)
	})

	t.Run("CaseInsensitive", func(t *testing.T) {
		f := newMergeTestFs(t, true)
		byName := mergedByName(t, f, [][]upstream.Entry{
			objectEntries(u0, "Foo", "bar"),
			objectEntries(u1, "foo", "BAR", "baz"),
		})
		require.Len(t, byName, 3)
		require.Len(t, byName["Foo"], 2)
		assert.Equal(t, "Foo", byName["Foo"][0].Remote())
		assert.Equal(t, "foo", byName["Foo"][1].Remote())
		assert.Same(t, u0, byName["Foo"][0].UpstreamFs())
		assert.Same(t, u1, byName["Foo"][1].UpstreamFs())
		require.Len(t, byName["bar"], 2)
		require.Len(t, byName["baz"], 1)
	})

	t.Run("CaseSensitive", func(t *testing.T) {
		byName := mergedByName(t, f, [][]upstream.Entry{
			objectEntries(u0, "Foo"),
			objectEntries(u1, "foo"),
		})
		assert.Len(t, byName, 2)
	})
}

// TestMergeDirEntriesRetention checks that keeping one merged entry does not
// keep the upstream entries of the other names alive.
func TestMergeDirEntriesRetention(t *testing.T) {
	const n = 1000
	f := newMergeTestFs(t, false)
	// merge drops its lists on return so only the kept entry and the weak
	// pointers outlive it.
	merge := func() (kept fs.DirEntry, ptrs []weak.Pointer[upstream.Object]) {
		u := &upstream.Fs{}
		names := make([]string, n)
		for i := range names {
			names[i] = fmt.Sprintf("file-%04d", i)
		}
		list := objectEntries(u, names...)
		ptrs = make([]weak.Pointer[upstream.Object], len(list))
		for i, e := range list {
			ptrs[i] = weak.Make(e.(*upstream.Object))
		}
		merged, err := f.mergeDirEntries([][]upstream.Entry{list})
		require.NoError(t, err)
		require.Len(t, merged, n)
		return merged[0], ptrs
	}
	kept, ptrs := merge()

	live := n
	for range 10 {
		runtime.GC()
		live = 0
		for _, w := range ptrs {
			if w.Value() != nil {
				live++
			}
		}
		if live <= 1 {
			break
		}
	}
	runtime.KeepAlive(kept)
	assert.LessOrEqual(t, live, 1, "upstream entries of other names are kept alive by one merged entry")
}

// benchmarkMergeLists builds three upstream lists of about n names in
// total, with about 5% of the names present in more than one upstream
func benchmarkMergeLists(n int) [][]upstream.Entry {
	ups := []*upstream.Fs{{}, {}, {}}
	names := make([][]string, len(ups))
	for i := range n {
		name := fmt.Sprintf("dir/file-%06d.mkv", i)
		u := i % len(ups)
		names[u] = append(names[u], name)
		switch i % 40 {
		case 0: // 2.5% in two upstreams
			next := (u + 1) % len(ups)
			names[next] = append(names[next], name)
		case 20: // 2.5% in three upstreams
			for _, next := range []int{(u + 1) % len(ups), (u + 2) % len(ups)} {
				names[next] = append(names[next], strings.Clone(name))
			}
		}
	}
	lists := make([][]upstream.Entry, len(ups))
	for i, u := range ups {
		lists[i] = objectEntries(u, names[i]...)
	}
	return lists
}

func BenchmarkMergeDirEntries(b *testing.B) {
	f := newMergeTestFs(b, false)
	lists := benchmarkMergeLists(50000)
	b.ReportAllocs()
	for b.Loop() {
		merged, err := f.mergeDirEntries(lists)
		if err != nil {
			b.Fatal(err)
		}
		if len(merged) != 50000 {
			b.Fatalf("got %d entries", len(merged))
		}
	}
}
