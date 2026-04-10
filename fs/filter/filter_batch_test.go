package filter

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fstest/mockobject"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeObjects(parent string, names []string) []fs.Object {
	objects := make([]fs.Object, len(names))
	for i, name := range names {
		objects[i] = mockobject.New(parent + name)
	}
	return objects
}

func sizedObject(remote string, size int) *mockobject.ContentMockObject {
	return mockobject.New(remote).WithContent(make([]byte, size), mockobject.SeekModeNone)
}

func timedObject(remote string, modTime time.Time) *mockobject.ContentMockObject {
	object := mockobject.New(remote).WithContent(nil, mockobject.SeekModeNone)
	if err := object.SetModTime(context.Background(), modTime); err != nil {
		panic(fmt.Sprintf("set modtime for %q: %v", remote, err))
	}
	return object
}

func addParsedRules(t *testing.T, f *Filter, specs []struct {
	glob    string
	include bool
}) {
	t.Helper()

	for _, spec := range specs {
		prefix := "- "
		if spec.include {
			prefix = "+ "
		}
		require.NoError(t, f.AddRule(prefix+spec.glob))
	}
}

func goldenBatchLeaves() []string {
	leaves := []string{
		"photo.jpg",
		"readme.txt",
		".hidden",
		"noext",
		"archive.tmp",
		"data.bin",
		"movie.mkv",
		"guide.pdf",
		"test.rclonelink",
	}
	return append([]string(nil), leaves...)
}

func TestIncludeObjectBatchEmpty(t *testing.T) {
	f, err := NewFilter(nil)
	require.NoError(t, err)

	var nilObjects []fs.Object
	assert.Empty(t, f.IncludeObjectBatch(context.Background(), "", nilObjects))
	assert.Empty(t, f.IncludeObjectBatch(context.Background(), "", []fs.Object{}))
}

func TestIncludeObjectBatchNoRules(t *testing.T) {
	f, err := NewFilter(nil)
	require.NoError(t, err)

	objects := makeObjects("media/", []string{"cover.jpg", "notes.txt", "archive.bin"})
	assert.Equal(t, []bool{true, true, true}, f.IncludeObjectBatch(context.Background(), "media/", objects))
}

func TestIncludeObjectBatchRules(t *testing.T) {
	f, err := NewFilter(nil)
	require.NoError(t, err)
	require.NoError(t, f.AddRule("+ *.jpg"))
	require.NoError(t, f.AddRule("- *"))

	objects := makeObjects("media/", []string{"cover.jpg", "notes.txt", "poster.jpg"})
	assert.Equal(t, []bool{true, false, true}, f.IncludeObjectBatch(context.Background(), "media/", objects))
}

func TestIncludeObjectBatchFilesFrom(t *testing.T) {
	f, err := NewFilter(nil)
	require.NoError(t, err)
	require.NoError(t, f.AddFile("docs/keep.txt"))
	require.NoError(t, f.AddRule("- *"))

	objects := makeObjects("docs/", []string{"keep.txt", "drop.txt"})
	assert.Equal(t, []bool{true, false}, f.IncludeObjectBatch(context.Background(), "docs/", objects))
}

func TestIncludeObjectBatchRootParent(t *testing.T) {
	f, err := NewFilter(nil)
	require.NoError(t, err)
	require.NoError(t, f.AddRule("+ *.jpg"))
	require.NoError(t, f.AddRule("- *"))

	objects := makeObjects("", []string{"root.jpg", "readme.txt", "banner.jpg"})
	assert.Equal(t, []bool{true, false, true}, f.IncludeObjectBatch(context.Background(), "", objects))
}

func TestIncludeObjectBatchSizeFilter(t *testing.T) {
	f, err := NewFilter(nil)
	require.NoError(t, err)
	f.Opt.MinSize = fs.SizeSuffix(10)
	require.NoError(t, f.AddRule("+ *.jpg"))
	require.NoError(t, f.AddRule("- *"))

	objects := []fs.Object{
		sizedObject("media/small.jpg", 5),
		sizedObject("media/boundary.jpg", 10),
		sizedObject("media/large.jpg", 20),
		sizedObject("media/skip.txt", 50),
	}
	assert.Equal(t, []bool{false, true, true, false}, f.IncludeObjectBatch(context.Background(), "media/", objects))
}

func TestIncludeObjectBatchModTimeFilter(t *testing.T) {
	f, err := NewFilter(nil)
	require.NoError(t, err)
	require.NoError(t, f.AddRule("+ *.jpg"))
	require.NoError(t, f.AddRule("- *"))

	cutoff := time.Date(2026, time.January, 10, 12, 0, 0, 0, time.UTC)
	f.ModTimeFrom = cutoff

	objects := []fs.Object{
		timedObject("media/old.jpg", cutoff.Add(-time.Hour)),
		timedObject("media/exact.jpg", cutoff),
		timedObject("media/new.jpg", cutoff.Add(time.Hour)),
		timedObject("media/skip.txt", cutoff.Add(2*time.Hour)),
	}
	assert.Equal(t, []bool{false, true, true, false}, f.IncludeObjectBatch(context.Background(), "media/", objects))
}

func TestIncludeObjectBatchLargeBatch(t *testing.T) {
	f, err := NewFilter(nil)
	require.NoError(t, err)
	require.NoError(t, f.AddRule("+ *.jpg"))
	require.NoError(t, f.AddRule("- *"))

	names := make([]string, 100)
	want := make([]bool, 100)
	for i := range names {
		if i%2 == 0 {
			names[i] = fmt.Sprintf("item-%03d.jpg", i)
			want[i] = true
		} else {
			names[i] = fmt.Sprintf("item-%03d.txt", i)
		}
	}

	objects := makeObjects("bulk/", names)
	assert.Equal(t, want, f.IncludeObjectBatch(context.Background(), "bulk/", objects))
}

func TestIncludeObjectBatchParityWithIncludeObject(t *testing.T) {
	type testCase struct {
		name      string
		parent    string
		configure func(t *testing.T, f *Filter)
		objects   []fs.Object
	}

	modTimeToCutoff := time.Date(2026, time.January, 12, 12, 0, 0, 0, time.UTC)
	testCases := []testCase{
		{
			name:   "simple include exclude",
			parent: "",
			configure: func(t *testing.T, f *Filter) {
				t.Helper()
				require.NoError(t, f.AddRule("+ *.jpg"))
				require.NoError(t, f.AddRule("- *"))
			},
			objects: makeObjects("", []string{"one.jpg", "two.txt", "three.jpg"}),
		},
		{
			name:   "mixed polarity production style",
			parent: "",
			configure: func(t *testing.T, f *Filter) {
				t.Helper()
				for _, rule := range []string{
					"- .*",
					"- *.tmp",
					"- *.bak",
					"+ keep/**",
					"+ reports/**",
					"+ team/**",
					"- *",
				} {
					require.NoError(t, f.AddRule(rule))
				}
			},
			objects: makeObjects("", []string{
				".hidden",
				"keep/photo.jpg",
				"keep/cache.tmp",
				"reports/q1.pdf",
				"team/notes.txt",
				"misc/readme.md",
				"draft.bak",
			}),
		},
		{
			name:   "size filters active",
			parent: "media/",
			configure: func(t *testing.T, f *Filter) {
				t.Helper()
				f.Opt.MinSize = fs.SizeSuffix(5)
				for _, rule := range []string{
					"- *.tmp",
					"+ media/**",
					"- *",
				} {
					require.NoError(t, f.AddRule(rule))
				}
			},
			objects: []fs.Object{
				sizedObject("media/tiny.bin", 4),
				sizedObject("media/song.mp3", 5),
				sizedObject("media/temp.tmp", 100),
				sizedObject("media/cover.jpg", 20),
			},
		},
		{
			name:   "non root parent",
			parent: "team/alpha/",
			configure: func(t *testing.T, f *Filter) {
				t.Helper()
				for _, rule := range []string{
					"- .*",
					"- *.tmp",
					"+ team/alpha/**",
					"+ team/beta/**",
					"- *",
				} {
					require.NoError(t, f.AddRule(rule))
				}
			},
			objects: makeObjects("team/alpha/", []string{"plan.txt", ".secret", "scratch.tmp", "sub/photo.jpg"}),
		},
		{
			name:   "max size filters active",
			parent: "media/",
			configure: func(t *testing.T, f *Filter) {
				t.Helper()
				f.Opt.MaxSize = fs.SizeSuffix(100)
				for _, rule := range []string{
					"+ media/**",
					"- *",
				} {
					require.NoError(t, f.AddRule(rule))
				}
			},
			objects: []fs.Object{
				sizedObject("media/small.bin", 50),
				sizedObject("media/boundary.bin", 100),
				sizedObject("media/large.bin", 200),
			},
		},
		{
			name:   "mod time to filters active",
			parent: "events/",
			configure: func(t *testing.T, f *Filter) {
				t.Helper()
				f.ModTimeTo = modTimeToCutoff
				for _, rule := range []string{
					"+ events/**",
					"- *",
				} {
					require.NoError(t, f.AddRule(rule))
				}
			},
			objects: []fs.Object{
				timedObject("events/past.jpg", modTimeToCutoff.Add(-time.Hour)),
				timedObject("events/exact.jpg", modTimeToCutoff),
				timedObject("events/future.jpg", modTimeToCutoff.Add(time.Hour)),
			},
		},
	}

	ctx := context.Background()
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := NewFilter(nil)
			require.NoError(t, err)
			tc.configure(t, f)

			scalar := make([]bool, len(tc.objects))
			for i, object := range tc.objects {
				scalar[i] = f.IncludeObject(ctx, object)
			}

			batch := f.IncludeObjectBatch(ctx, tc.parent, tc.objects)
			require.Len(t, batch, len(scalar))
			assert.Equal(t, scalar, batch)
			for i, object := range tc.objects {
				assert.Equalf(t, scalar[i], batch[i], "parity mismatch for %q", object.Remote())
			}
		})
	}
}

func TestIncludeObjectBatchHashFilter(t *testing.T) {
	f, err := NewFilter(nil)
	require.NoError(t, err)
	require.NoError(t, f.AddRule("+ media/**"))
	require.NoError(t, f.AddRule("- *"))
	f.hashFilterN = 3
	f.hashFilterK = 1

	names := make([]string, 30)
	for i := range names {
		names[i] = fmt.Sprintf("item-%02d.bin", i)
	}
	objects := makeObjects("media/", names)

	ctx := context.Background()
	scalar := make([]bool, len(objects))
	for i, object := range objects {
		scalar[i] = f.IncludeObject(ctx, object)
	}

	batch := f.IncludeObjectBatch(ctx, "media/", objects)
	require.Equal(t, scalar, batch)

	included := 0
	for _, allowed := range batch {
		if allowed {
			included++
		}
	}
	require.Greater(t, included, 0, "expected hash filter to keep at least one object")
	require.Less(t, included, len(batch), "expected hash filter to exclude at least one object")
}

func TestIncludeObjectBatchGoldenParity(t *testing.T) {
	parents := []string{
		"",
		"media-a/",
		"media-b/",
		"learning/",
		"learning/provider-a/",
		"sports/",
		".downloads/",
		"tmp/",
		"other/",
	}

	specs := parseFilterRulesString(t, standardFilterRulesV2)
	ctx := context.Background()
	for _, parent := range parents {
		name := parent
		if name == "" {
			name = "root"
		}

		t.Run(name, func(t *testing.T) {
			f, err := NewFilter(nil)
			require.NoError(t, err)
			addParsedRules(t, f, specs)

			objects := makeObjects(parent, goldenBatchLeaves())
			scalar := make([]bool, len(objects))
			for i, object := range objects {
				scalar[i] = f.IncludeObject(ctx, object)
			}

			batch := f.IncludeObjectBatch(ctx, parent, objects)
			require.Len(t, batch, len(scalar))
			assert.Equal(t, scalar, batch)
			for i, object := range objects {
				assert.Equalf(t, scalar[i], batch[i], "parity mismatch for %q", object.Remote())
			}
		})
	}
}

func TestCopyFilterStateIsolation(t *testing.T) {
	t.Run("rule mutation on copy does not affect original", func(t *testing.T) {
		original, err := NewFilter(nil)
		require.NoError(t, err)
		require.NoError(t, original.AddRule("+ *.jpg"))
		originalDump := original.DumpFilters()

		copied := new(Filter)
		copyFilterState(copied, original)
		require.NoError(t, copied.AddRule("- *.png"))

		assert.Equal(t, originalDump, original.DumpFilters())
		assert.NotEqual(t, original.DumpFilters(), copied.DumpFilters())
	})

	t.Run("files map independence", func(t *testing.T) {
		original, err := NewFilter(nil)
		require.NoError(t, err)
		require.NoError(t, original.AddFile("docs/keep.txt"))

		copied := new(Filter)
		copyFilterState(copied, original)
		require.NoError(t, copied.AddFile("other/new.txt"))

		_, originalHasFile := original.files["other/new.txt"]
		_, originalHasDir := original.dirs["other"]
		assert.False(t, originalHasFile)
		assert.False(t, originalHasDir)
		_, copiedHasFile := copied.files["other/new.txt"]
		_, copiedHasDir := copied.dirs["other"]
		assert.True(t, copiedHasFile)
		assert.True(t, copiedHasDir)
	})

	t.Run("batchEval is nil on copy", func(t *testing.T) {
		original, err := NewFilter(nil)
		require.NoError(t, err)
		require.NoError(t, original.AddRule("+ *.jpg"))
		require.NoError(t, original.AddRule("- *"))
		_ = original.IncludeObjectBatch(context.Background(), "media/", makeObjects("media/", []string{"cover.jpg"}))
		require.NotNil(t, original.batchEval)

		copied := new(Filter)
		copyFilterState(copied, original)
		assert.Nil(t, copied.batchEval)
	})

	t.Run("fused pointer is nil on copy", func(t *testing.T) {
		original, err := NewFilter(nil)
		require.NoError(t, err)
		require.NoError(t, original.AddRule("+ *.jpg"))
		assert.True(t, original.IncludeRemote("cover.jpg"))
		require.NotNil(t, original.fileRules.fused.Load())

		copied := new(Filter)
		copyFilterState(copied, original)
		assert.Nil(t, copied.fileRules.fused.Load())
	})
}

func TestBatchEvalInvalidation(t *testing.T) {
	t.Run("add invalidates batchEval", func(t *testing.T) {
		f, err := NewFilter(nil)
		require.NoError(t, err)
		require.NoError(t, f.AddRule("+ *.jpg"))
		require.NoError(t, f.AddRule("- *"))

		objects := makeObjects("", []string{"notes.txt", "photo.jpg"})
		assert.Equal(t, []bool{false, true}, f.IncludeObjectBatch(context.Background(), "", objects))
		require.NotNil(t, f.batchEval)

		require.NoError(t, f.AddRule("- *.txt"))
		assert.Nil(t, f.batchEval)
		assert.Equal(t, []bool{false, true}, f.IncludeObjectBatch(context.Background(), "", objects))
		require.NotNil(t, f.batchEval)
	})

	t.Run("inactive filter skips batchEval", func(t *testing.T) {
		f, err := NewFilter(nil)
		require.NoError(t, err)

		objects := makeObjects("", []string{"notes.txt", "photo.jpg"})
		assert.Equal(t, []bool{true, true}, f.IncludeObjectBatch(context.Background(), "", objects))
		assert.Nil(t, f.batchEval)
	})

	t.Run("clear invalidates batchEval", func(t *testing.T) {
		f, err := NewFilter(nil)
		require.NoError(t, err)
		require.NoError(t, f.AddRule("+ *.jpg"))
		require.NoError(t, f.AddRule("- *"))

		objects := makeObjects("", []string{"notes.txt", "photo.jpg"})
		assert.Equal(t, []bool{false, true}, f.IncludeObjectBatch(context.Background(), "", objects))
		require.NotNil(t, f.batchEval)

		f.Clear()
		assert.Nil(t, f.batchEval)
		assert.Equal(t, []bool{true, true}, f.IncludeObjectBatch(context.Background(), "", objects))
		assert.Nil(t, f.batchEval)
	})

	t.Run("reset rule invalidates batchEval", func(t *testing.T) {
		f, err := NewFilter(nil)
		require.NoError(t, err)
		require.NoError(t, f.AddRule("- *.txt"))

		objects := makeObjects("", []string{"notes.txt", "photo.jpg"})
		assert.Equal(t, []bool{false, true}, f.IncludeObjectBatch(context.Background(), "", objects))
		require.NotNil(t, f.batchEval)

		require.NoError(t, f.AddRule("!"))
		assert.Nil(t, f.batchEval)
		assert.Equal(t, []bool{true, true}, f.IncludeObjectBatch(context.Background(), "", objects))
		assert.Nil(t, f.batchEval)
	})
}

func TestFusedInvalidationOnRuleAdd(t *testing.T) {
	f, err := NewFilter(nil)
	require.NoError(t, err)

	assert.True(t, f.IncludeRemote("notes.txt"))
	require.NotNil(t, f.fileRules.fused.Load(), "fused should be built after IncludeRemote")

	require.NoError(t, f.AddRule("- *.txt"))
	assert.Nil(t, f.fileRules.fused.Load(), "fused must be invalidated after AddRule")

	assert.False(t, f.IncludeRemote("notes.txt"), "notes.txt should now be excluded")
	assert.True(t, f.IncludeRemote("photo.jpg"), "photo.jpg should still be included")
}

func TestCopyFilterStateOverwritesBatchEval(t *testing.T) {
	dst, err := NewFilter(nil)
	require.NoError(t, err)
	require.NoError(t, dst.AddRule("+ *.jpg"))
	require.NoError(t, dst.AddRule("- *"))

	r1 := dst.IncludeObjectBatch(context.Background(), "", makeObjects("", []string{"a.jpg", "b.txt"}))
	assert.Equal(t, []bool{true, false}, r1)

	dst.batchMu.Lock()
	require.NotNil(t, dst.batchEval, "dst batchEval should be initialized")
	dst.batchMu.Unlock()

	src, err := NewFilter(nil)
	require.NoError(t, err)
	require.NoError(t, src.AddRule("+ *.txt"))
	require.NoError(t, src.AddRule("- *"))

	copyFilterState(dst, src)

	dst.batchMu.Lock()
	assert.Nil(t, dst.batchEval, "batchEval must be nil after copyFilterState")
	dst.batchMu.Unlock()

	r2 := dst.IncludeObjectBatch(context.Background(), "", makeObjects("", []string{"a.jpg", "b.txt"}))
	assert.Equal(t, []bool{false, true}, r2, "new rules should take effect after copyFilterState")
}
