package local

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/filter"
	"github.com/rclone/rclone/lib/encoder"
	"github.com/rclone/rclone/vfs"
)

// Minimal fakes for local listing and scheduler tests (bypass real filesystem).
type fakeDirEntry struct {
	name string
	typ  os.FileMode
}

func (f fakeDirEntry) Name() string      { return f.name }
func (f fakeDirEntry) IsDir() bool       { return f.typ.IsDir() }
func (f fakeDirEntry) Type() os.FileMode { return f.typ }
func (f fakeDirEntry) Info() (os.FileInfo, error) {
	return fakeFileInfo{name: f.name, mode: f.typ}, nil
}

type fakeFileInfo struct {
	name string
	mode os.FileMode
}

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() os.FileMode  { return f.mode }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeFileInfo) Sys() any           { return nil }

func TestStopTimer_DrainsFiredTimerBeforeReset(t *testing.T) {
	timer := time.NewTimer(time.Millisecond)
	t.Cleanup(func() {
		stopTimer(timer)
	})

	select {
	case <-timer.C:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for initial timer fire")
	}

	stopTimer(timer)
	timer.Reset(25 * time.Millisecond)

	select {
	case <-timer.C:
		t.Fatal("timer fired immediately after reset")
	case <-time.After(5 * time.Millisecond):
	}

	select {
	case <-timer.C:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reset timer fire")
	}
}

func TestNewFs_EagerlyCreatesListStatScheduler(t *testing.T) {
	f, _ := newTestLocalFs(t)
	require.NotNil(t, f.statScheduler)
	assert.Equal(t, statSchedulerDefaultWorkers, f.statScheduler.Snapshot().Workers)
}

func TestCleanRemoteUtf8Gate(t *testing.T) {
	invalidName := string([]byte{'b', 'a', 'd', '-', 0xff})
	const prefix = "subdir/"

	tests := []struct {
		name     string
		call     func(*Fs, string) string
		disabled func(*testing.T, *Fs, string, string)
		enabled  func(*testing.T, *Fs, string, string)
	}{
		{
			name: "cleanRemote",
			call: func(f *Fs, invalid string) string {
				return f.cleanRemote("", invalid)
			},
			disabled: func(t *testing.T, _ *Fs, remote, _ string) {
				assert.NotEmpty(t, remote)
			},
			enabled: func(t *testing.T, f *Fs, remote, invalid string) {
				assert.Equal(t, f.opt.Enc.ToStandardName(invalid), remote)
			},
		},
		{
			name: "cleanRemoteWithPrefix",
			call: func(f *Fs, invalid string) string {
				return f.cleanRemoteWithPrefix(prefix, invalid)
			},
			disabled: func(t *testing.T, _ *Fs, remote, invalid string) {
				assert.Equal(t, prefix+invalid, remote)
			},
			enabled: func(t *testing.T, f *Fs, remote, invalid string) {
				expected := prefix + f.opt.Enc.ToStandardName(invalid)
				assert.Equal(t, expected, remote)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("disabled", func(t *testing.T) {
				f, _ := newTestLocalFs(t)
				f.opt.Enc &^= encoder.EncodeInvalidUtf8
				f.validateUtf8 = false
				f.warned = make(map[string]struct{})

				remote := tc.call(f, invalidName)

				tc.disabled(t, f, remote, invalidName)
				assert.Empty(t, f.warned)
			})

			t.Run("enabled", func(t *testing.T) {
				f, _ := newTestLocalFs(t)
				f.opt.Enc |= encoder.EncodeInvalidUtf8
				f.validateUtf8 = true
				f.warned = make(map[string]struct{})

				remote := tc.call(f, invalidName)
				tc.call(f, invalidName)

				tc.enabled(t, f, remote, invalidName)
				assert.Contains(t, f.warned, remote)
				assert.Len(t, f.warned, 1)
			})
		})
	}
}
func TestListedConstructorsUseProvidedLocalPath(t *testing.T) {
	f, root := newTestLocalFs(t)

	filePath := filepath.Join(root, "real.txt")
	writeTestFile(t, root, "real.txt")
	obj, err := f.newListedObject("virtual.txt", filePath, false, fakeFileInfo{name: "real.txt"})
	require.NoError(t, err)
	listedObj, ok := obj.(*Object)
	require.True(t, ok)
	assert.Equal(t, "virtual.txt", listedObj.remote)
	assert.Equal(t, filePath, listedObj.path)

	dirPath := filepath.Join(root, "real-dir")
	writeTestDir(t, root, "real-dir")
	dir := f.newListedDirectory("virtual-dir", dirPath, fakeFileInfo{name: "real-dir", mode: os.ModeDir})
	assert.Equal(t, "virtual-dir", dir.remote)
	assert.Equal(t, dirPath, dir.path)
}

func TestListCachedFileInfos_FilteredBatchUsesSharedScheduler(t *testing.T) {
	f, root := newTestLocalFs(t)
	for i := 0; i < statSchedulerDefaultMicroBatchSize+4; i++ {
		writeTestFile(t, root, fakeEntryName("item", i))
	}
	scheduler := f.statScheduler

	fd, err := os.Open(root)
	require.NoError(t, err)

	var statCalls atomic.Int64
	var fis []statFileInfo
	err = f.listCachedFileInfos(context.Background(), fd, nil, "", func(entry os.DirEntry) (cachedDirEntry, bool) {
		if entry.Name() != "item-000" && entry.Name() != "item-001" {
			return cachedDirEntry{}, false
		}
		return cachedDirEntry{DirEntry: entry, remote: entry.Name()}, true
	}, func(entry *cachedDirEntry, nameBuf []byte) (os.FileInfo, []byte, error) {
		statCalls.Add(1)
		return fakeFileInfo{name: entry.Name(), mode: entry.Type()}, nameBuf, nil
	}, func(_ []cachedDirEntry, batch []statFileInfo) error {
		fis = append(fis, append([]statFileInfo(nil), batch...)...)
		return nil
	})
	require.NoError(t, err)
	assert.Same(t, scheduler, f.statScheduler, "filtered batches should use the eagerly created shared scheduler")
	assert.Equal(t, int64(2), statCalls.Load())
	assert.Equal(t, []string{"item-000", "item-001"}, statFileInfoNames(fis))
}

func TestListCachedFileInfos_ReusesFsSchedulerAcrossListings(t *testing.T) {
	f, root := newTestLocalFs(t)
	for i := 0; i < statSchedulerDefaultMicroBatchSize+4; i++ {
		writeTestFile(t, root, fakeEntryName("item", i))
	}
	scheduler := f.statScheduler

	run := func() []statFileInfo {
		t.Helper()

		fd, err := os.Open(root)
		require.NoError(t, err)

		var fis []statFileInfo
		err = f.listCachedFileInfos(context.Background(), fd, nil, "", nil, func(entry *cachedDirEntry, nameBuf []byte) (os.FileInfo, []byte, error) {
			return fakeFileInfo{name: entry.Name(), mode: entry.Type()}, nameBuf, nil
		}, func(_ []cachedDirEntry, batch []statFileInfo) error {
			fis = append(fis, append([]statFileInfo(nil), batch...)...)
			return nil
		})
		require.NoError(t, err)
		return fis
	}

	first := run()
	second := run()
	assert.Same(t, scheduler, f.statScheduler, "listings on the same Fs should reuse the shared scheduler")
	assert.Equal(t, statFileInfoNames(first), statFileInfoNames(second))
}

func TestListCachedFileInfos_WatchdogRetryPreservesForwardProgressAfterMaxWorkerLeak(t *testing.T) {
	f, root := newTestLocalFs(t)
	const entryCount = statSchedulerDefaultMicroBatchSize + 2
	for i := 0; i < entryCount; i++ {
		writeTestFile(t, root, fakeEntryName("item", i))
	}

	f.statScheduler.Close()
	f.statScheduler = newStatScheduler(f, statSchedulerOptions{
		Workers:          1,
		MaxWorkers:       1,
		QueueDepth:       entryCount,
		LeaseTimeout:     40 * time.Millisecond,
		RetryBackoff:     5 * time.Millisecond,
		WatchdogInterval: 10 * time.Millisecond,
		WarnAfter:        time.Second,
		ReplaceAfter:     50 * time.Millisecond,
	})
	t.Cleanup(f.statScheduler.Close)

	fd, err := os.Open(root)
	require.NoError(t, err)

	firstStarted := make(chan struct{}, 1)
	retryStarted := make(chan struct{}, 1)
	releaseFirst := make(chan struct{})
	done := make(chan error, 1)
	var calls atomic.Int64
	processed := 0

	go func() {
		done <- f.listCachedFileInfos(context.Background(), fd, nil, "", nil, func(entry *cachedDirEntry, nameBuf []byte) (os.FileInfo, []byte, error) {
			switch calls.Add(1) {
			case 1:
				select {
				case firstStarted <- struct{}{}:
				default:
				}
				<-releaseFirst
			case 2:
				select {
				case retryStarted <- struct{}{}:
				default:
				}
			}
			return fakeFileInfo{name: entry.Name(), mode: entry.Type()}, nameBuf, nil
		}, func(_ []cachedDirEntry, batch []statFileInfo) error {
			for i := range batch {
				if batch[i].fi != nil {
					processed++
				}
			}
			return nil
		})
	}()

	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		close(releaseFirst)
		t.Fatal("timed out waiting for stuck stat work to start")
	}

	select {
	case <-retryStarted:
	case <-time.After(time.Second):
		close(releaseFirst)
		t.Fatal("timed out waiting for watchdog retry work to start")
	}

	close(releaseFirst)

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for watchdog retry to finish listing")
	}

	assert.GreaterOrEqual(t, calls.Load(), int64(2))
	assert.Equal(t, entryCount, processed)

	require.Eventually(t, func() bool {
		snapshot := f.statScheduler.Snapshot()
		return snapshot.Workers == 1 && snapshot.Timeouts == 1 && snapshot.Retries == 1 && snapshot.StaleCompletions == 1
	}, time.Second, 10*time.Millisecond)
}

func TestListCachedFileInfos_CancellationStopsScheduledBatch(t *testing.T) {
	f, root := newTestLocalFs(t)
	for i := 0; i < statSchedulerDefaultMicroBatchSize+4; i++ {
		writeTestFile(t, root, fakeEntryName("item", i))
	}

	ctx, cancel := context.WithCancel(context.Background())
	f.statScheduler.Close()
	f.statScheduler = newStatScheduler(f, statSchedulerOptions{
		Workers:          1,
		MaxWorkers:       1,
		QueueDepth:       statSchedulerDefaultMicroBatchSize + 4,
		LeaseTimeout:     time.Second,
		RetryBackoff:     time.Millisecond,
		WatchdogInterval: time.Second,
		WarnAfter:        time.Second,
		ReplaceAfter:     time.Second,
	})
	t.Cleanup(f.statScheduler.Close)

	fd, err := os.Open(root)
	require.NoError(t, err)

	started := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		err := f.listCachedFileInfos(ctx, fd, nil, "", nil, func(entry *cachedDirEntry, nameBuf []byte) (os.FileInfo, []byte, error) {
			select {
			case started <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return nil, nameBuf, nil
		}, func(_ []cachedDirEntry, _ []statFileInfo) error {
			return nil
		})
		done <- err
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for scheduled stat work to start")
	}

	cancel()

	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for scheduled batch cancellation")
	}
}

func TestListCachedFileInfos_Cancellation(t *testing.T) {
	f, root := newTestLocalFs(t)
	for i := 0; i < statSchedulerDefaultMicroBatchSize+4; i++ {
		writeTestFile(t, root, fakeEntryName("item", i))
	}

	ctx, cancel := context.WithCancel(context.Background())
	f.statScheduler.Close()
	f.statScheduler = newStatScheduler(f, statSchedulerOptions{
		Workers:          1,
		MaxWorkers:       1,
		QueueDepth:       statSchedulerDefaultMicroBatchSize + 4,
		LeaseTimeout:     time.Second,
		RetryBackoff:     time.Millisecond,
		WatchdogInterval: time.Second,
		WarnAfter:        time.Second,
		ReplaceAfter:     time.Second,
	})
	t.Cleanup(f.statScheduler.Close)

	fd, err := os.Open(root)
	require.NoError(t, err)

	started := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		err := f.listCachedFileInfos(ctx, fd, nil, "", nil, func(entry *cachedDirEntry, nameBuf []byte) (os.FileInfo, []byte, error) {
			select {
			case started <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return nil, nameBuf, ctx.Err()
		}, nil)
		done <- err
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for listCachedFileInfos stat work to start")
	}

	cancel()

	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for listCachedFileInfos cancellation")
	}
}

func TestListCachedFileInfos_ConsumeErrorDoesNotWedgeScheduler(t *testing.T) {
	f, root := newTestLocalFs(t)
	writeTestFile(t, root, "alpha.txt")
	writeTestFile(t, root, "beta.txt")
	writeTestFile(t, root, "gamma.txt")

	fd, err := os.Open(root)
	require.NoError(t, err)

	scheduler := f.statScheduler
	consumeErr := errors.New("consume failed")
	consumeCalls := 0
	processed := 0

	err = f.listCachedFileInfos(context.Background(), fd, nil, "", nil, func(entry *cachedDirEntry, nameBuf []byte) (os.FileInfo, []byte, error) {
		fi, err := entry.Info()
		return fi, nameBuf, err
	}, func(_ []cachedDirEntry, fis []statFileInfo) error {
		consumeCalls++
		for i := range fis {
			if fis[i].fi == nil {
				continue
			}
			processed++
			return consumeErr
		}
		return nil
	})

	require.ErrorIs(t, err, consumeErr)
	assert.Equal(t, 1, consumeCalls)
	assert.Equal(t, 1, processed)

	snapshot := scheduler.Snapshot()
	assert.Equal(t, statSchedulerDefaultWorkers, snapshot.Workers)
	assert.Zero(t, snapshot.ActiveLeases)
	assert.Zero(t, snapshot.Timeouts)
	assert.Zero(t, snapshot.Retries)
	assert.Zero(t, snapshot.StaleCompletions)

	entries, err := f.List(context.Background(), "")
	require.NoError(t, err)
	assert.Equal(t, []string{"alpha.txt", "beta.txt", "gamma.txt"}, dirEntryRemotes(entries))
}

func TestList_FilterRegularFileCases(t *testing.T) {
	tests := []struct {
		name  string
		rules []string
		want  []string
	}{
		{
			name:  "include regular file",
			rules: []string{"+ *.txt", "- *"},
			want:  []string{"keep.txt"},
		},
		{
			name:  "exclude regular file",
			rules: []string{"- *.log", "+ *"},
			want:  []string{"keep.txt"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, root := newTestLocalFs(t)
			writeTestFile(t, root, "keep.txt")
			writeTestFile(t, root, "skip.log")

			ctx, fi := filter.AddConfig(context.Background())
			for _, rule := range tc.rules {
				require.NoError(t, fi.AddRule(rule))
			}
			ctx = filter.SetUseFilter(ctx, true)

			entries, err := f.List(ctx, "")
			require.NoError(t, err)
			assert.Equal(t, tc.want, dirEntryRemotes(entries))
		})
	}
}

// Verifies that exclude-if-present marker files survive both filter sites
// (statFunc and object-construction loop) even when rules would exclude them.
// Without the slices.Contains guard, ListContainsExcludeFile would miss the marker.
func TestList_ExcludeFileMarkerPreserved(t *testing.T) {
	f, root := newTestLocalFs(t)
	writeTestFile(t, root, ".nomedia")
	writeTestFile(t, root, "photo.jpg")

	ctx, fi := filter.AddConfig(context.Background())
	fi.Opt.ExcludeFile = []string{".nomedia"}
	require.NoError(t, fi.AddRule("- .nomedia"))
	ctx = filter.SetUseFilter(ctx, true)

	entries, err := f.List(ctx, "")
	require.NoError(t, err)
	assert.Equal(t, []string{".nomedia", "photo.jpg"}, dirEntryRemotes(entries))
}

func TestList_DirFilterSkip(t *testing.T) {
	f, root := newTestLocalFs(t)
	writeTestDir(t, root, "keep")
	writeTestDir(t, root, "skip")

	ctx, fi := filter.AddConfig(context.Background())
	require.NoError(t, fi.AddRule("+ keep/**"))
	require.NoError(t, fi.AddRule("- *"))
	ctx = filter.SetUseFilter(ctx, true)

	entries, err := f.List(ctx, "")
	require.NoError(t, err)
	assert.Equal(t, []string{"keep"}, dirEntryRemotes(entries))
}

// Verifies that directory pre-filter is DISABLED when ExcludeFile is set
// (IncludeDirectory would do I/O). Both dirs must survive.
func TestList_DirFilterSkipWithExcludeFile(t *testing.T) {
	f, root := newTestLocalFs(t)
	writeTestDir(t, root, "keep")
	writeTestDir(t, root, "skip")

	ctx, fi := filter.AddConfig(context.Background())
	fi.Opt.ExcludeFile = []string{".nomedia"}
	require.NoError(t, fi.AddRule("+ keep/**"))
	require.NoError(t, fi.AddRule("- *"))
	ctx = filter.SetUseFilter(ctx, true)

	entries, err := f.List(ctx, "")
	require.NoError(t, err)
	assert.Equal(t, []string{"keep", "skip"}, dirEntryRemotes(entries))
}

func TestList_NestedSubdirKeepsRemotePrefix(t *testing.T) {
	f, root := newTestLocalFs(t)
	writeTestFile(t, root, "subdir/foo.txt")
	writeTestDir(t, root, "subdir/child")

	entries, err := f.List(context.Background(), "subdir")
	require.NoError(t, err)

	type listedEntry struct {
		kind string
		path string
	}

	got := make(map[string]listedEntry, len(entries))
	for _, entry := range entries {
		switch entry := entry.(type) {
		case *Object:
			got[entry.Remote()] = listedEntry{
				kind: "file",
				path: entry.path,
			}
		case *Directory:
			got[entry.Remote()] = listedEntry{
				kind: "dir",
				path: entry.path,
			}
		default:
			t.Fatalf("unexpected entry type %T", entry)
		}
	}

	assert.Equal(t, map[string]listedEntry{
		"subdir/child": {
			kind: "dir",
			path: filepath.Join(root, "subdir", "child"),
		},
		"subdir/foo.txt": {
			kind: "file",
			path: filepath.Join(root, "subdir", "foo.txt"),
		},
	}, got)
}

func TestList_NestedSubdirFilterKeepsRemotePrefix(t *testing.T) {
	f, root := newTestLocalFs(t)
	writeTestFile(t, root, "subdir/foo.txt")
	writeTestFile(t, root, "subdir/skip.log")
	writeTestDir(t, root, "subdir/child")

	ctx, fi := filter.AddConfig(context.Background())
	require.NoError(t, fi.AddRule("+ subdir/foo.txt"))
	require.NoError(t, fi.AddRule("- *"))
	ctx = filter.SetUseFilter(ctx, true)

	entries, err := f.List(ctx, "subdir")
	require.NoError(t, err)
	require.Len(t, entries, 1)

	obj, ok := entries[0].(*Object)
	require.True(t, ok)
	assert.Equal(t, "subdir/foo.txt", obj.Remote())
	assert.Equal(t, filepath.Join(root, "subdir", "foo.txt"), obj.path)
	assert.Equal(t, []string{"subdir/foo.txt"}, dirEntryRemotes(entries))
}

func TestList_ConcurrentCallersShareSchedulerWithoutCrossContamination(t *testing.T) {
	f, root := newTestLocalFs(t)

	const callers = 8
	expected := make(map[string][]string, callers)
	for i := 0; i < callers; i++ {
		dir := fmt.Sprintf("dir-%d", i)
		fileName := fmt.Sprintf("file-%d.txt", i)
		childName := fmt.Sprintf("child-%d", i)
		writeTestFile(t, root, filepath.Join(dir, fileName))
		writeTestDir(t, root, filepath.Join(dir, childName))
		writeTestFile(t, root, filepath.Join(dir, childName, "nested.txt"))
		expected[dir] = []string{
			dir + "/" + childName,
			dir + "/" + fileName,
		}
	}

	var wg sync.WaitGroup
	errCh := make(chan error, callers)

	for dir, want := range expected {
		dir, want := dir, append([]string(nil), want...)
		wg.Add(1)
		go func() {
			defer wg.Done()

			entries, err := f.List(context.Background(), dir)
			if err != nil {
				errCh <- fmt.Errorf("List(%q): %w", dir, err)
				return
			}

			got := dirEntryRemotes(entries)
			if !assert.ObjectsAreEqual(want, got) {
				errCh <- fmt.Errorf("List(%q) remotes mismatch: want %v got %v", dir, want, got)
			}
		}()
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		require.NoError(t, err)
	}
}

func TestLocalFs_ShutdownExposesFeatureAndIsIdempotent(t *testing.T) {
	f, _ := newTestLocalFs(t)

	require.NotNil(t, f.Features().Shutdown)
	require.NoError(t, f.Features().Shutdown(context.Background()))
	require.NoError(t, f.Features().Shutdown(context.Background()))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, f.Features().Shutdown(ctx), context.Canceled)
}

// End-to-end: VFS _readDir propagates SetUseFilter through to the local backend.
// Uses global filter state (not context-scoped) since VFS uses context.TODO().
// WARNING: mutates global filter.Opt. Do not use t.Parallel().
func TestVFSReadDirAll_FilterPropagation(t *testing.T) {
	f, root := newTestLocalFs(t)
	writeTestFile(t, root, "keep.txt")
	writeTestFile(t, root, "skip.log")

	savedOpt := filter.Opt
	t.Cleanup(func() {
		filter.Opt = savedOpt
		require.NoError(t, filter.Reload(context.Background()))
	})

	filter.Opt = savedOpt
	filter.Opt.IncludeRule = []string{"*.txt"}
	filter.Opt.ExcludeRule = []string{"*"}
	require.NoError(t, filter.Reload(context.Background()))

	vfsFS := vfs.New(context.Background(), f, nil)
	t.Cleanup(vfsFS.Shutdown)

	rootDir, err := vfsFS.Root()
	require.NoError(t, err)

	nodes, err := rootDir.ReadDirAll()
	require.NoError(t, err)

	names := make([]string, 0, len(nodes))
	for _, node := range nodes {
		names = append(names, node.Name())
	}
	sort.Strings(names)
	assert.Equal(t, []string{"keep.txt"}, names)
}

func fakeEntries(names ...string) []os.DirEntry {
	entries := make([]os.DirEntry, 0, len(names))
	for _, name := range names {
		entries = append(entries, fakeDirEntry{name: name})
	}
	return entries
}

func fakeEntryName(prefix string, idx int) string {
	return fmt.Sprintf("%s-%03d", prefix, idx)
}

func statFileInfoNames(fis []statFileInfo) []string {
	names := make([]string, 0, len(fis))
	for _, fi := range fis {
		names = append(names, fi.Name())
	}
	sort.Strings(names)
	return names
}

func newTestLocalFs(t *testing.T) (*Fs, string) {
	t.Helper()
	root := t.TempDir()
	f, err := NewFs(context.Background(), "local", root, configmap.Simple{})
	require.NoError(t, err)
	localFs, ok := f.(*Fs)
	require.True(t, ok)
	t.Cleanup(func() {
		localFs.statScheduler.Close()
	})
	return localFs, root
}

func writeTestFile(t *testing.T, root, name string) {
	t.Helper()
	path := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(name), 0o600))
}

func writeTestDir(t *testing.T, root, name string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(root, name), 0o755))
}

func dirEntryRemotes(entries fs.DirEntries) []string {
	remotes := make([]string, 0, len(entries))
	for _, entry := range entries {
		remotes = append(remotes, entry.Remote())
	}
	sort.Strings(remotes)
	return remotes
}
