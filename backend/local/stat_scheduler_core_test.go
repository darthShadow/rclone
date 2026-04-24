package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStatSchedulerOptions_WithDefaults(t *testing.T) {
	opts := (statSchedulerOptions{}).withDefaults()
	assert.Equal(t, statSchedulerDefaultQueueDepth, opts.QueueDepth)
	assert.Equal(t, time.Duration(statTimeout)*time.Second, opts.LeaseTimeout)
	assert.Equal(t, statSchedulerDefaultRetryBackoff, opts.RetryBackoff)
	assert.Equal(t, watchdogInterval, opts.WatchdogInterval)
	assert.Equal(t, stuckWarnTimeout, opts.WarnAfter)
	assert.Equal(t, stuckReplaceTimeout, opts.ReplaceAfter)
}

func TestListControllerOptions_WithDefaults(t *testing.T) {
	opts := (listControllerOptions{}).withDefaults()
	assert.Equal(t, statSchedulerDefaultMicroBatchSize, opts.MicroBatchSize)
}

func TestTranslateSchedulerErr(t *testing.T) {
	sysErr := &os.PathError{Op: "lstat", Path: "/tmp/x", Err: syscall.ENOENT}
	plainErr := errors.New("plain")
	bugErr := fmt.Errorf("wrapped: %w", errStatSchedulerBug)

	tests := []struct {
		name            string
		err             error
		wantSame        error
		wantIs          error
		wantNotExist    bool
		wantPathErr     bool
		wantPathErrOp   string
		wantPathErrPath string
	}{
		{
			name:         "syscall_passthrough",
			err:          sysErr,
			wantSame:     sysErr,
			wantNotExist: true,
			wantPathErr:  true,
		},
		{
			name:            "context_canceled_path_error_preserves_identity",
			err:             context.Canceled,
			wantIs:          context.Canceled,
			wantPathErr:     true,
			wantPathErrOp:   "lstat",
			wantPathErrPath: "/x",
		},
		{
			name:        "bug_sentinel_path_error_preserves_identity",
			err:         bugErr,
			wantIs:      errStatSchedulerBug,
			wantPathErr: true,
		},
		{
			name:        "default_path_error_preserves_original",
			err:         plainErr,
			wantIs:      plainErr,
			wantPathErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := translateSchedulerErr("lstat", "/x", tc.err)
			if tc.wantSame != nil {
				assert.Same(t, tc.wantSame, got)
			}
			if tc.wantIs != nil {
				require.ErrorIs(t, got, tc.wantIs)
			}
			assert.Equal(t, tc.wantNotExist, os.IsNotExist(got))
			if tc.wantPathErr {
				var pathErr *os.PathError
				require.ErrorAs(t, got, &pathErr)
				if tc.wantPathErrOp != "" {
					assert.Equal(t, tc.wantPathErrOp, pathErr.Op)
				}
				if tc.wantPathErrPath != "" {
					assert.Equal(t, tc.wantPathErrPath, pathErr.Path)
				}
			}
		})
	}
}

func TestFsStatRoutesThroughScheduler(t *testing.T) {
	t.Run("lstat_path_uses_scheduler_shutdown", func(t *testing.T) {
		f, root := newTestLocalFs(t)
		writeTestFile(t, root, "alpha.txt")

		f.statScheduler.Close()

		_, err := f.Stat(context.Background(), "", "alpha.txt")
		require.Error(t, err)
		require.ErrorIs(t, err, context.Canceled)
		var pathErr *os.PathError
		require.ErrorAs(t, err, &pathErr)
		assert.Equal(t, "lstat", pathErr.Op)
		assert.False(t, os.IsNotExist(err))
	})

	t.Run("follow_symlink_stat_path_uses_scheduler", func(t *testing.T) {
		f, _ := newTestLocalFs(t)
		f.opt.FollowSymlinks = true
		statErr := errors.New("scheduled stat failed")
		f.lstat = func(path string) (os.FileInfo, error) {
			return fakeFileInfo{name: "link", mode: os.ModeSymlink}, nil
		}
		f.statScheduler.Close()
		f.statScheduler = newStatScheduler(f, statSchedulerOptions{
			Workers:          1,
			MaxWorkers:       1,
			QueueDepth:       2,
			LeaseTimeout:     time.Second,
			RetryBackoff:     time.Millisecond,
			WatchdogInterval: time.Second,
			WarnAfter:        time.Second,
			ReplaceAfter:     time.Second,
		})
		f.statScheduler.statFn = func(path string) (os.FileInfo, error) {
			return nil, statErr
		}

		_, err := f.Stat(context.Background(), "", "link")
		require.Error(t, err)
		require.ErrorIs(t, err, statErr)
		var pathErr *os.PathError
		require.ErrorAs(t, err, &pathErr)
		assert.Equal(t, "stat", pathErr.Op)
		assert.False(t, os.IsNotExist(err))
	})
}

func TestListController_ProcessBatch_PreservesScheduledResultSlots(t *testing.T) {
	scheduler := newStatScheduler(nil, statSchedulerOptions{
		Workers:          1,
		MaxWorkers:       1,
		QueueDepth:       1,
		LeaseTimeout:     time.Second,
		RetryBackoff:     time.Millisecond,
		WatchdogInterval: time.Second,
		WarnAfter:        time.Second,
		ReplaceAfter:     time.Second,
	})
	defer scheduler.Close()

	controller := newListController(nil, scheduler, listControllerOptions{
		MicroBatchSize: 4,
	})

	batch := newReadResult()
	batch.entries = fakeEntries("alpha", "skip", "gamma")
	batch.err = io.EOF

	var filtered []cachedDirEntry
	var fis []statFileInfo
	err := controller.ProcessBatch(context.Background(), &batch, func(entry os.DirEntry) (cachedDirEntry, bool) {
		return cachedDirEntry{
			DirEntry: entry,
			remote:   "remote/" + entry.Name(),
		}, true
	}, func(entry *cachedDirEntry, nameBuf []byte) (os.FileInfo, []byte, error) {
		if entry.Name() == "skip" {
			return nil, nameBuf, nil
		}
		return fakeFileInfo{name: entry.Name(), mode: entry.Type()}, nameBuf, nil
	}, func(batchEntries []cachedDirEntry, batchFis []statFileInfo) error {
		filtered = append([]cachedDirEntry(nil), batchEntries...)
		fis = append([]statFileInfo(nil), batchFis...)
		return nil
	})

	require.NoError(t, err)
	require.Len(t, filtered, 3)
	require.Len(t, fis, 3)
	assert.Equal(t, "alpha", fis[0].Name())
	assert.Equal(t, "remote/alpha", filtered[0].Remote())
	assert.Nil(t, fis[1].fi)
	assert.Equal(t, "remote/skip", filtered[1].Remote())
	assert.Equal(t, "gamma", fis[2].Name())
	assert.Equal(t, "remote/gamma", filtered[2].Remote())
}

func TestListController_ProcessBatchIgnoresCallerCancellation(t *testing.T) {
	tests := []struct {
		name             string
		cancelBeforeCall bool
		cancelDuringStat bool
	}{
		{name: "pre_call_cancel_returns_result", cancelBeforeCall: true},
		{name: "mid_call_cancel_returns_result", cancelDuringStat: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scheduler := newStatScheduler(nil, statSchedulerOptions{
				Workers:          1,
				MaxWorkers:       1,
				QueueDepth:       4,
				LeaseTimeout:     time.Second,
				RetryBackoff:     time.Millisecond,
				WatchdogInterval: time.Second,
				WarnAfter:        time.Second,
				ReplaceAfter:     time.Second,
			})
			defer scheduler.Close()

			controller := newListController(nil, scheduler, listControllerOptions{
				MicroBatchSize: 1,
			})
			batch := newReadResult()
			batch.entries = fakeEntries("alpha")

			started := make(chan struct{}, 1)
			release := make(chan struct{})
			type batchResult struct {
				entries []cachedDirEntry
				fis     []statFileInfo
				err     error
			}
			resultCh := make(chan batchResult, 1)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelBeforeCall {
				cancel()
			}

			go func() {
				result := batchResult{}
				err := controller.ProcessBatch(ctx, &batch, func(entry os.DirEntry) (cachedDirEntry, bool) {
					return cachedDirEntry{
						DirEntry: entry,
						remote:   "remote/" + entry.Name(),
					}, true
				}, func(entry *cachedDirEntry, nameBuf []byte) (os.FileInfo, []byte, error) {
					select {
					case started <- struct{}{}:
					default:
					}
					<-release
					return fakeFileInfo{name: entry.Name(), mode: entry.Type()}, nameBuf, nil
				}, func(entries []cachedDirEntry, fis []statFileInfo) error {
					result.entries = append([]cachedDirEntry(nil), entries...)
					result.fis = append([]statFileInfo(nil), fis...)
					return nil
				})
				result.err = err
				resultCh <- result
			}()

			select {
			case <-started:
			case <-time.After(time.Second):
				close(release)
				t.Fatal("timed out waiting for ProcessBatch stat work to start")
			}

			if tc.cancelDuringStat {
				cancel()
			}

			select {
			case result := <-resultCh:
				close(release)
				t.Fatalf("ProcessBatch returned before the blocked stat was released: err=%v entries=%d fis=%d", result.err, len(result.entries), len(result.fis))
			case <-time.After(25 * time.Millisecond):
			}

			close(release)

			select {
			case result := <-resultCh:
				require.NoError(t, result.err)
				require.Len(t, result.entries, 1)
				require.Len(t, result.fis, 1)
				assert.Equal(t, "remote/alpha", result.entries[0].Remote())
				assert.Equal(t, "alpha", result.fis[0].Name())
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for ProcessBatch to return a result after caller cancellation")
			}
		})
	}
}

func TestRunStatFunc(t *testing.T) {
	testErr := errors.New("boom")
	entry := &cachedDirEntry{
		DirEntry: fakeDirEntry{name: "alpha"},
	}

	tests := []struct {
		name        string
		statFunc    func(*cachedDirEntry, []byte) (os.FileInfo, []byte, error)
		wantName    string
		wantErr     string
		wantWrapped error
	}{
		{
			name: "panic error",
			statFunc: func(*cachedDirEntry, []byte) (os.FileInfo, []byte, error) {
				panic(testErr)
			},
			wantErr:     `stat panic for "alpha": stat scheduler bug: boom`,
			wantWrapped: testErr,
		},
		{
			name: "panic string",
			statFunc: func(*cachedDirEntry, []byte) (os.FileInfo, []byte, error) {
				panic("boom")
			},
			wantErr: `stat panic for "alpha": stat scheduler bug: boom`,
		},
		{
			name: "normal return",
			statFunc: func(entry *cachedDirEntry, nameBuf []byte) (os.FileInfo, []byte, error) {
				return fakeFileInfo{name: entry.Name()}, nameBuf, nil
			},
			wantName: "alpha",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fi, _, err := runStatFunc(entry, tc.statFunc, make([]byte, 0, 16))
			assert.Equal(t, tc.wantName, fi.Name())
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.EqualError(t, err, tc.wantErr)
			require.ErrorIs(t, err, errStatSchedulerBug)
			if tc.wantWrapped != nil {
				require.ErrorIs(t, err, tc.wantWrapped)
			}
		})
	}
}

func TestStatScheduler_StaleCompletionDiscardedAndRetried(t *testing.T) {
	scheduler := newStatScheduler(nil, statSchedulerOptions{
		Workers:          1,
		MaxWorkers:       2,
		QueueDepth:       4,
		LeaseTimeout:     40 * time.Millisecond,
		RetryBackoff:     5 * time.Millisecond,
		WatchdogInterval: 10 * time.Millisecond,
		WarnAfter:        50 * time.Millisecond,
		ReplaceAfter:     60 * time.Millisecond,
	})
	defer scheduler.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	entries := []cachedDirEntry{
		{DirEntry: fakeDirEntry{name: "alpha"}},
		{DirEntry: fakeDirEntry{name: "beta"}},
	}

	firstStarted := make(chan struct{}, 1)
	releaseFirst := make(chan struct{})
	secondComplete := make(chan struct{}, 1)
	var calls atomic.Int64

	batch := newBatchController(ctx, entries, invalidStatDirFD, 4, func(entry *cachedDirEntry, nameBuf []byte) (os.FileInfo, []byte, error) {
		call := calls.Add(1)
		if call == 1 {
			firstStarted <- struct{}{}
			<-releaseFirst
		}
		if entry.Name() == "beta" && call >= 3 {
			select {
			case secondComplete <- struct{}{}:
			default:
			}
		}
		return fakeFileInfo{name: entry.Name(), mode: entry.Type()}, nameBuf, nil
	})
	defer batch.Close()

	require.NoError(t, batch.Schedule(scheduler))

	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the first lease to start")
	}

	fisCh := make(chan []statFileInfo, 1)
	errCh := make(chan error, 1)
	go func() {
		_, fis, err := batch.Wait(ctx)
		if err != nil {
			errCh <- err
			return
		}
		fisCh <- fis
	}()

	select {
	case <-secondComplete:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the retry lease to finish")
	}

	close(releaseFirst)

	select {
	case err := <-errCh:
		t.Fatalf("unexpected batch error: %v", err)
	case fis := <-fisCh:
		require.Len(t, fis, 2)
		assert.Equal(t, "alpha", fis[0].Name())
		assert.Equal(t, "beta", fis[1].Name())
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for batch completion")
	}

	require.Eventually(t, func() bool {
		return scheduler.Snapshot().StaleCompletions == 1 && scheduler.Snapshot().Retries == 1 && scheduler.Snapshot().Timeouts == 1
	}, time.Second, 10*time.Millisecond)
}

func TestBatchController_WaitReturnsBeforeTimedOutLeaseRetiresWhenResultsStayUncompacted(t *testing.T) {
	releaseOriginalCh := make(chan struct{})
	var releaseOriginalOnce sync.Once
	releaseOriginal := func() {
		releaseOriginalOnce.Do(func() {
			close(releaseOriginalCh)
		})
	}

	scheduler := newStatScheduler(nil, statSchedulerOptions{
		Workers:          1,
		MaxWorkers:       2,
		QueueDepth:       4,
		LeaseTimeout:     40 * time.Millisecond,
		RetryBackoff:     5 * time.Millisecond,
		WatchdogInterval: 10 * time.Millisecond,
		WarnAfter:        50 * time.Millisecond,
		ReplaceAfter:     60 * time.Millisecond,
	})
	defer func() {
		releaseOriginal()
		scheduler.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	originalStarted := make(chan struct{}, 1)
	retryBetaDone := make(chan struct{}, 1)
	waitReturned := make(chan struct{})
	staleReadName := make(chan string, 1)
	var calls atomic.Int64

	entries := []cachedDirEntry{
		{DirEntry: fakeDirEntry{name: "alpha"}},
		{DirEntry: fakeDirEntry{name: "beta"}},
	}
	batch := newBatchController(ctx, entries, invalidStatDirFD, 2, func(entry *cachedDirEntry, nameBuf []byte) (os.FileInfo, []byte, error) {
		call := calls.Add(1)
		name := entry.Name()
		if call == 1 {
			originalStarted <- struct{}{}
			<-releaseOriginalCh
			<-waitReturned
			staleReadName <- entry.Name()
			return fakeFileInfo{name: name, mode: entry.Type()}, nameBuf, nil
		}
		if name == "alpha" {
			return nil, nameBuf, nil
		}
		select {
		case retryBetaDone <- struct{}{}:
		default:
		}
		return fakeFileInfo{name: name, mode: entry.Type()}, nameBuf, nil
	})
	defer batch.Close()

	require.NoError(t, batch.Schedule(scheduler))

	select {
	case <-originalStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for original lease to start")
	}

	type batchResult struct {
		entries []cachedDirEntry
		fis     []statFileInfo
		err     error
	}
	resultCh := make(chan batchResult, 1)
	go func() {
		entries, fis, err := batch.Wait(ctx)
		resultCh <- batchResult{entries: entries, fis: fis, err: err}
	}()

	select {
	case <-retryBetaDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for retry lease to produce beta")
	}

	select {
	case result := <-resultCh:
		close(waitReturned)
		require.NoError(t, result.err)
		require.Len(t, result.entries, 2)
		require.Len(t, result.fis, 2)
		assert.Equal(t, "alpha", result.entries[0].Name())
		assert.Equal(t, "beta", result.entries[1].Name())
		assert.Nil(t, result.fis[0].fi)
		assert.Equal(t, "beta", result.fis[1].Name())

		var consumed []string
		for i := range result.fis {
			if result.fis[i].fi == nil {
				continue
			}
			consumed = append(consumed, result.entries[i].Name())
		}
		assert.Equal(t, []string{"beta"}, consumed)

		batch.mu.Lock()
		timedOutLeases := len(batch.timedOutLeases)
		retiredLeaseID := batch.parts[0].retiredLeaseID
		batch.mu.Unlock()
		assert.Equal(t, 1, timedOutLeases)
		assert.Zero(t, retiredLeaseID)
	case <-time.After(250 * time.Millisecond):
		t.Fatal("Wait did not return promptly after the retry lease published")
	}

	releaseOriginal()

	select {
	case name := <-staleReadName:
		assert.Equal(t, "alpha", name)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stale worker read")
	}

	require.Eventually(t, func() bool {
		snapshot := scheduler.Snapshot()
		if snapshot.Timeouts != 1 || snapshot.Retries != 1 || snapshot.StaleCompletions != 1 {
			return false
		}
		batch.mu.Lock()
		defer batch.mu.Unlock()
		return len(batch.timedOutLeases) == 0 && batch.parts[0].retiredLeaseID == 1
	}, time.Second, 10*time.Millisecond)
}

func TestBatchController_PublishRejectsLateCompletionAfterCancel(t *testing.T) {
	tests := []struct {
		name   string
		cancel func(*testing.T, *batchController)
	}{
		{
			name: "explicit_cancel_rejects_publish",
			cancel: func(t *testing.T, batch *batchController) {
				t.Helper()
				batch.cancel()
			},
		},
		{
			name: "wait_ctx_cancel_rejects_publish",
			cancel: func(t *testing.T, batch *batchController) {
				t.Helper()

				waitCtx, cancel := context.WithCancel(context.Background())
				errCh := make(chan error, 1)
				go func() {
					_, _, err := batch.Wait(waitCtx)
					errCh <- err
				}()

				cancel()

				select {
				case err := <-errCh:
					require.ErrorIs(t, err, context.Canceled)
				case <-time.After(time.Second):
					t.Fatal("timed out waiting for Wait to cancel the batch")
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			batch := newBatchController(context.Background(), []cachedDirEntry{
				{DirEntry: fakeDirEntry{name: "alpha"}},
			}, invalidStatDirFD, 1, func(entry *cachedDirEntry, nameBuf []byte) (os.FileInfo, []byte, error) {
				return fakeFileInfo{name: entry.Name(), mode: entry.Type()}, nameBuf, nil
			})
			defer batch.Close()

			job := &statJob{}
			require.True(t, batch.acquirePendingLease(0, job))

			tc.cancel(t, batch)

			published, completed := batch.publish(0, job.leaseID, []statFileInfo{
				{fi: fakeFileInfo{name: "late"}},
			}, nil)
			assert.False(t, published)
			assert.False(t, completed)

			batch.mu.Lock()
			assert.True(t, batch.canceled)
			assert.Nil(t, batch.results[0].fi)
			assert.Equal(t, statMicroBatchLeased, batch.parts[0].state)
			batch.mu.Unlock()
		})
	}
}

func TestListController_ProcessBatch_ForfeitsScratchWhileTimedOutWorkerStillRuns(t *testing.T) {
	scheduler := newStatScheduler(nil, statSchedulerOptions{
		Workers:          1,
		MaxWorkers:       2,
		QueueDepth:       4,
		LeaseTimeout:     150 * time.Millisecond,
		RetryBackoff:     5 * time.Millisecond,
		WatchdogInterval: 10 * time.Millisecond,
		WarnAfter:        time.Second,
		ReplaceAfter:     225 * time.Millisecond,
	})
	defer scheduler.Close()

	controller := newListController(nil, scheduler, listControllerOptions{
		MicroBatchSize: 1,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	type batchResult struct {
		entries []cachedDirEntry
		fis     []statFileInfo
		err     error
	}

	batchAStarted := make(chan struct{}, 1)
	releaseBatchA := make(chan struct{})
	batchBStarted := make(chan struct{}, 1)
	releaseBatchB := make(chan struct{})
	var batchACalls atomic.Int64
	var releaseAOnce sync.Once
	var releaseBOnce sync.Once
	releaseA := func() {
		releaseAOnce.Do(func() {
			close(releaseBatchA)
		})
	}
	releaseB := func() {
		releaseBOnce.Do(func() {
			close(releaseBatchB)
		})
	}

	batchAName := "batch-a"
	batchARemote := "remote/batch-a"
	batchBName := "batch-b"
	batchBRemote := "remote/batch-b"

	batchA := newReadResult()
	batchA.entries = fakeEntries("alpha")
	batchAResultCh := make(chan batchResult, 1)
	go func() {
		result := batchResult{}
		err := controller.ProcessBatch(ctx, &batchA, func(entry os.DirEntry) (cachedDirEntry, bool) {
			return cachedDirEntry{DirEntry: entry, remote: batchARemote}, true
		}, func(entry *cachedDirEntry, nameBuf []byte) (os.FileInfo, []byte, error) {
			if batchACalls.Add(1) == 1 {
				select {
				case batchAStarted <- struct{}{}:
				default:
				}
				<-releaseBatchA
			}
			return fakeFileInfo{name: batchAName, mode: entry.Type()}, nameBuf, nil
		}, func(entries []cachedDirEntry, fis []statFileInfo) error {
			result.entries = append([]cachedDirEntry(nil), entries...)
			result.fis = append([]statFileInfo(nil), fis...)
			return nil
		})
		result.err = err
		batchAResultCh <- result
	}()

	select {
	case <-batchAStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for batch A to start")
	}

	var batchAResult batchResult
	select {
	case batchAResult = <-batchAResultCh:
	case <-time.After(time.Second):
		releaseA()
		t.Fatal("timed out waiting for batch A retry completion")
	}

	require.NoError(t, batchAResult.err)
	require.Len(t, batchAResult.fis, 1)
	assert.Equal(t, batchAName, batchAResult.fis[0].Name())
	assert.Equal(t, batchARemote, batchAResult.entries[0].Remote())

	batchB := newReadResult()
	batchB.entries = fakeEntries("beta")
	batchBResultCh := make(chan batchResult, 1)
	go func() {
		result := batchResult{}
		err := controller.ProcessBatch(ctx, &batchB, func(entry os.DirEntry) (cachedDirEntry, bool) {
			return cachedDirEntry{DirEntry: entry, remote: batchBRemote}, true
		}, func(entry *cachedDirEntry, nameBuf []byte) (os.FileInfo, []byte, error) {
			select {
			case batchBStarted <- struct{}{}:
			default:
			}
			<-releaseBatchB
			return fakeFileInfo{name: batchBName, mode: entry.Type()}, nameBuf, nil
		}, func(entries []cachedDirEntry, fis []statFileInfo) error {
			result.entries = append([]cachedDirEntry(nil), entries...)
			result.fis = append([]statFileInfo(nil), fis...)
			return nil
		})
		result.err = err
		batchBResultCh <- result
	}()

	select {
	case <-batchBStarted:
	case <-time.After(time.Second):
		releaseA()
		releaseB()
		t.Fatal("timed out waiting for batch B to start")
	}

	releaseA()

	select {
	case result := <-batchBResultCh:
		releaseB()
		t.Fatalf("batch B returned before its own worker was released: fis=%v err=%v", result.fis, result.err)
	case <-time.After(50 * time.Millisecond):
	}

	releaseB()

	var batchBResult batchResult
	select {
	case batchBResult = <-batchBResultCh:
	case <-time.After(time.Second):
		releaseB()
		t.Fatal("timed out waiting for batch B completion")
	}

	require.NoError(t, batchBResult.err)
	require.Len(t, batchBResult.fis, 1)
	assert.Equal(t, batchBName, batchBResult.fis[0].Name())
	assert.Equal(t, batchBRemote, batchBResult.entries[0].Remote())

	require.Eventually(t, func() bool {
		snapshot := scheduler.Snapshot()
		return snapshot.Timeouts == 1 && snapshot.Retries == 1 && snapshot.StaleCompletions == 1
	}, time.Second, 10*time.Millisecond)
}

func TestStatScheduler_WaitReturnsErrorAfterOuterExecuteLeasePanic(t *testing.T) {
	scheduler := newStatScheduler(nil, statSchedulerOptions{
		Workers:          1,
		MaxWorkers:       1,
		QueueDepth:       4,
		LeaseTimeout:     time.Second,
		RetryBackoff:     time.Millisecond,
		WatchdogInterval: time.Second,
		WarnAfter:        time.Second,
		ReplaceAfter:     time.Second,
	})
	defer scheduler.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	panicBatch := newBatchController(ctx, []cachedDirEntry{{DirEntry: fakeDirEntry{name: "alpha"}}}, invalidStatDirFD, 1, func(entry *cachedDirEntry, nameBuf []byte) (os.FileInfo, []byte, error) {
		return fakeFileInfo{name: entry.Name()}, nameBuf, nil
	})
	defer panicBatch.Close()

	panicJob := scheduler.getJob()
	require.True(t, panicBatch.acquirePendingLease(0, panicJob))
	panicJob.end = 2
	require.NoError(t, scheduler.enqueueNormal(ctx, panicJob))

	type batchResult struct {
		entries []cachedDirEntry
		fis     []statFileInfo
		err     error
	}
	resultCh := make(chan batchResult, 1)
	go func() {
		entries, fis, err := panicBatch.Wait(ctx)
		resultCh <- batchResult{entries: entries, fis: fis, err: err}
	}()

	select {
	case result := <-resultCh:
		require.Error(t, result.err)
		assert.ErrorContains(t, result.err, "stat scheduler panic")
		assert.ErrorContains(t, result.err, "index out of range")
		require.Len(t, result.entries, 1)
		require.Len(t, result.fis, 1)
		assert.Nil(t, result.fis[0].fi)
	case <-time.After(250 * time.Millisecond):
		t.Fatal("Wait did not return promptly after outer executeLease panic")
	}

	require.Eventually(t, func() bool {
		snapshot := scheduler.Snapshot()
		if snapshot.ActiveLeases != 0 || snapshot.Timeouts != 0 || snapshot.Retries != 0 || snapshot.StaleCompletions != 0 {
			return false
		}
		panicBatch.mu.Lock()
		defer panicBatch.mu.Unlock()
		return panicBatch.remainingParts == 0 && len(panicBatch.timedOutLeases) == 0 && panicBatch.parts[0].state == statMicroBatchDone
	}, time.Second, 10*time.Millisecond)

	followupBatch := newBatchController(ctx, []cachedDirEntry{{DirEntry: fakeDirEntry{name: "beta"}}}, invalidStatDirFD, 1, func(entry *cachedDirEntry, nameBuf []byte) (os.FileInfo, []byte, error) {
		return fakeFileInfo{name: entry.Name()}, nameBuf, nil
	})
	defer followupBatch.Close()

	require.NoError(t, followupBatch.Schedule(scheduler))
	_, fis, err := followupBatch.Wait(ctx)
	require.NoError(t, err)
	require.Len(t, fis, 1)
	assert.Equal(t, "beta", fis[0].Name())
}

func TestStatScheduler_ReplacementWorkerIgnoresLeakedWorkersAndBackoffGrows(t *testing.T) {
	originalNewRetryTimer := newStatSchedulerRetryTimer
	var retryTimerMu sync.Mutex
	var retryTimerDelays []time.Duration
	newStatSchedulerRetryTimer = func(delay time.Duration) *time.Timer {
		retryTimerMu.Lock()
		retryTimerDelays = append(retryTimerDelays, delay)
		retryTimerMu.Unlock()
		return time.NewTimer(time.Millisecond)
	}
	t.Cleanup(func() {
		newStatSchedulerRetryTimer = originalNewRetryTimer
	})

	scheduler := newStatScheduler(nil, statSchedulerOptions{
		Workers:          1,
		MaxWorkers:       1,
		QueueDepth:       4,
		LeaseTimeout:     40 * time.Millisecond,
		RetryBackoff:     5 * time.Millisecond,
		WatchdogInterval: 5 * time.Millisecond,
		WarnAfter:        time.Second,
		ReplaceAfter:     50 * time.Millisecond,
	})
	defer scheduler.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	firstStarted := make(chan struct{}, 1)
	secondStarted := make(chan struct{}, 1)
	thirdStarted := make(chan struct{}, 1)
	staleFinished := make(chan int, 2)
	releaseFirst := make(chan struct{})
	releaseSecond := make(chan struct{})
	releaseThird := make(chan struct{})
	var attempts atomic.Int64

	gateBatch := newBatchController(ctx, []cachedDirEntry{{DirEntry: fakeDirEntry{name: "gate"}}}, invalidStatDirFD, 1, func(entry *cachedDirEntry, nameBuf []byte) (os.FileInfo, []byte, error) {
		switch attempt := attempts.Add(1); attempt {
		case 1:
			firstStarted <- struct{}{}
			<-releaseFirst
			staleFinished <- 1
		case 2:
			secondStarted <- struct{}{}
			<-releaseSecond
			staleFinished <- 2
		case 3:
			thirdStarted <- struct{}{}
			<-releaseThird
		default:
			t.Fatalf("unexpected attempt %d", attempt)
		}
		return fakeFileInfo{name: entry.Name()}, nameBuf, nil
	})
	defer gateBatch.Close()
	require.NoError(t, gateBatch.Schedule(scheduler))

	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the first worker attempt to start")
	}

	type batchResult struct {
		fis []statFileInfo
		err error
	}
	resultCh := make(chan batchResult, 1)
	go func() {
		_, fis, err := gateBatch.Wait(ctx)
		resultCh <- batchResult{fis: fis, err: err}
	}()

	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the second worker attempt to start")
	}

	select {
	case <-thirdStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the third worker attempt to start")
	}

	require.Eventually(t, func() bool {
		snapshot := scheduler.Snapshot()
		return snapshot.Workers == 1 && snapshot.ActiveLeases == 3
	}, time.Second, 5*time.Millisecond)

	gateBatch.mu.Lock()
	retries := gateBatch.parts[0].retries
	gateBatch.mu.Unlock()
	assert.Equal(t, 2, retries)
	require.Eventually(t, func() bool {
		retryTimerMu.Lock()
		defer retryTimerMu.Unlock()
		return len(retryTimerDelays) == 2
	}, time.Second, 5*time.Millisecond)
	retryTimerMu.Lock()
	assert.Equal(t, []time.Duration{5 * time.Millisecond, 10 * time.Millisecond}, retryTimerDelays)
	retryTimerMu.Unlock()
	assert.Equal(t, statSchedulerMaxRetryBackoff, statSchedulerRetryBackoff(10*time.Minute, 2))

	close(releaseThird)

	select {
	case result := <-resultCh:
		require.NoError(t, result.err)
		require.Len(t, result.fis, 1)
		assert.Equal(t, "gate", result.fis[0].Name())
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for batch completion after third attempt")
	}

	close(releaseFirst)
	close(releaseSecond)

	for i := 0; i < 2; i++ {
		select {
		case <-staleFinished:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for stale worker attempt to unwind")
		}
	}

	require.Eventually(t, func() bool {
		snapshot := scheduler.Snapshot()
		if snapshot.Timeouts != 2 || snapshot.Retries != 2 || snapshot.StaleCompletions != 2 {
			return false
		}
		gateBatch.mu.Lock()
		defer gateBatch.mu.Unlock()
		return len(gateBatch.timedOutLeases) == 0
	}, time.Second, 10*time.Millisecond)
}

func TestStatScheduler_NormalAndRetryQueuesBothDrain(t *testing.T) {
	scheduler := newStatScheduler(nil, statSchedulerOptions{
		Workers:          1,
		MaxWorkers:       1,
		QueueDepth:       8,
		LeaseTimeout:     time.Second,
		RetryBackoff:     time.Millisecond,
		WatchdogInterval: time.Second,
		WarnAfter:        time.Second,
		ReplaceAfter:     time.Second,
	})
	defer scheduler.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	gateStarted := make(chan struct{}, 1)
	releaseGate := make(chan struct{})
	var orderMu sync.Mutex
	var order []string
	record := func(name string) {
		orderMu.Lock()
		order = append(order, name)
		orderMu.Unlock()
	}

	gateBatch := newBatchController(ctx, []cachedDirEntry{{DirEntry: fakeDirEntry{name: "gate"}}}, invalidStatDirFD, 1, func(entry *cachedDirEntry, nameBuf []byte) (os.FileInfo, []byte, error) {
		record(entry.Name())
		gateStarted <- struct{}{}
		<-releaseGate
		return fakeFileInfo{name: entry.Name()}, nameBuf, nil
	})
	defer gateBatch.Close()
	require.NoError(t, gateBatch.Schedule(scheduler))

	select {
	case <-gateStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for gate job to start")
	}

	retryBatch := newBatchController(ctx, []cachedDirEntry{{DirEntry: fakeDirEntry{name: "retry"}}}, invalidStatDirFD, 1, func(entry *cachedDirEntry, nameBuf []byte) (os.FileInfo, []byte, error) {
		record(entry.Name())
		return fakeFileInfo{name: entry.Name()}, nameBuf, nil
	})
	defer retryBatch.Close()
	retryBatch.mu.Lock()
	retryBatch.parts[0].state = statMicroBatchRetryPending
	retryBatch.parts[0].retries = 1
	retryBatch.mu.Unlock()
	retryJob := &statJob{}
	ok := retryBatch.acquireRetryLease(0, retryJob)
	require.True(t, ok)
	require.NoError(t, scheduler.enqueueRetry(ctx, retryJob))

	normalBatch := newBatchController(ctx, []cachedDirEntry{{DirEntry: fakeDirEntry{name: "normal"}}}, invalidStatDirFD, 1, func(entry *cachedDirEntry, nameBuf []byte) (os.FileInfo, []byte, error) {
		record(entry.Name())
		return fakeFileInfo{name: entry.Name()}, nameBuf, nil
	})
	defer normalBatch.Close()
	normalJob := &statJob{}
	ok = normalBatch.acquirePendingLease(0, normalJob)
	require.True(t, ok)
	require.NoError(t, scheduler.enqueueNormal(ctx, normalJob))

	close(releaseGate)

	_, _, err := gateBatch.Wait(ctx)
	require.NoError(t, err)
	_, _, err = normalBatch.Wait(ctx)
	require.NoError(t, err)
	_, _, err = retryBatch.Wait(ctx)
	require.NoError(t, err)

	orderMu.Lock()
	defer orderMu.Unlock()
	require.Len(t, order, 3)
	assert.Equal(t, "gate", order[0])
	assert.ElementsMatch(t, []string{"normal", "retry"}, order[1:])
}

func TestStatScheduler_SubmitOneHappyPath(t *testing.T) {
	scheduler := newStatScheduler(nil, statSchedulerOptions{
		Workers:          1,
		MaxWorkers:       1,
		QueueDepth:       4,
		LeaseTimeout:     time.Second,
		RetryBackoff:     time.Millisecond,
		WatchdogInterval: time.Second,
		WarnAfter:        time.Second,
		ReplaceAfter:     time.Second,
	})
	defer scheduler.Close()

	scheduler.statFn = func(path string) (os.FileInfo, error) {
		return fakeFileInfo{name: "stat:" + path}, nil
	}
	scheduler.lstatFn = func(path string) (os.FileInfo, error) {
		return fakeFileInfo{name: "lstat:" + path, mode: os.ModeSymlink}, nil
	}

	tests := []struct {
		name  string
		path  string
		lstat bool
		want  string
	}{
		{name: "stat", path: "alpha", want: "stat:alpha"},
		{name: "lstat", path: "beta", lstat: true, want: "lstat:beta"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			info, err := scheduler.SubmitOne(context.Background(), tc.path, tc.lstat)
			require.NoError(t, err)
			require.NotNil(t, info)
			assert.Equal(t, tc.want, info.Name())
		})
	}
}

func TestStatScheduler_SubmitOneIgnoresCallerCancellation(t *testing.T) {
	tests := []struct {
		name             string
		cancelBeforeCall bool
		cancelDuringStat bool
	}{
		{name: "pre_call_cancel_returns_result", cancelBeforeCall: true},
		{name: "mid_call_cancel_returns_result", cancelDuringStat: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scheduler := newStatScheduler(nil, statSchedulerOptions{
				Workers:          1,
				MaxWorkers:       1,
				QueueDepth:       4,
				LeaseTimeout:     time.Second,
				RetryBackoff:     time.Millisecond,
				WatchdogInterval: time.Second,
				WarnAfter:        time.Second,
				ReplaceAfter:     time.Second,
			})
			defer scheduler.Close()

			started := make(chan struct{}, 1)
			release := make(chan struct{})
			var releaseOnce sync.Once
			releaseWorker := func() {
				releaseOnce.Do(func() {
					close(release)
				})
			}
			scheduler.statFn = func(path string) (os.FileInfo, error) {
				select {
				case started <- struct{}{}:
				default:
				}
				<-release
				return fakeFileInfo{name: path}, nil
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelBeforeCall {
				cancel()
			}

			type submitOneResult struct {
				info os.FileInfo
				err  error
			}
			resultCh := make(chan submitOneResult, 1)
			go func() {
				info, err := scheduler.SubmitOne(ctx, "alpha", false)
				resultCh <- submitOneResult{info: info, err: err}
			}()

			select {
			case <-started:
			case <-time.After(time.Second):
				releaseWorker()
				t.Fatal("timed out waiting for standalone stat to start")
			}

			if tc.cancelDuringStat {
				cancel()
			}

			select {
			case result := <-resultCh:
				releaseWorker()
				t.Fatalf("SubmitOne returned before the blocked stat was released: err=%v info=%v", result.err, result.info)
			case <-time.After(25 * time.Millisecond):
			}

			releaseWorker()

			select {
			case result := <-resultCh:
				require.NoError(t, result.err)
				require.NotNil(t, result.info)
				assert.Equal(t, "alpha", result.info.Name())
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for SubmitOne to return a result after caller cancellation")
			}
		})
	}
}

func TestStatScheduler_SubmitOneLeaseTimeoutIgnoresCallerDeadline(t *testing.T) {
	tests := []struct {
		name   string
		newCtx func() (context.Context, context.CancelFunc)
	}{
		{
			name: "expired_before_submit_retries_to_success",
			newCtx: func() (context.Context, context.CancelFunc) {
				return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			},
		},
		{
			name: "mid_call_deadline_retries_to_success",
			newCtx: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 10*time.Millisecond)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scheduler := newStatScheduler(nil, statSchedulerOptions{
				Workers:          1,
				MaxWorkers:       1,
				QueueDepth:       4,
				LeaseTimeout:     40 * time.Millisecond,
				RetryBackoff:     5 * time.Millisecond,
				WatchdogInterval: 5 * time.Millisecond,
				WarnAfter:        time.Second,
				ReplaceAfter:     50 * time.Millisecond,
			})
			defer scheduler.Close()

			started := make(chan struct{}, 1)
			retryStarted := make(chan struct{}, 1)
			release := make(chan struct{})
			var releaseOnce sync.Once
			releaseWorker := func() {
				releaseOnce.Do(func() {
					close(release)
				})
			}
			defer releaseWorker()
			var calls atomic.Int64
			scheduler.statFn = func(path string) (os.FileInfo, error) {
				call := calls.Add(1)
				if call == 1 {
					select {
					case started <- struct{}{}:
					default:
					}
					<-release
					return fakeFileInfo{name: "stale:" + path}, nil
				}
				select {
				case retryStarted <- struct{}{}:
				default:
				}
				return fakeFileInfo{name: "retry:" + path}, nil
			}

			ctx, cancel := tc.newCtx()
			defer cancel()

			type submitResult struct {
				info os.FileInfo
				err  error
			}
			resultCh := make(chan submitResult, 1)
			go func() {
				info, err := scheduler.SubmitOne(ctx, "alpha", false)
				resultCh <- submitResult{info: info, err: err}
			}()

			select {
			case <-started:
			case <-time.After(time.Second):
				releaseWorker()
				t.Fatal("timed out waiting for standalone stat to start")
			}

			select {
			case <-retryStarted:
			case <-time.After(time.Second):
				releaseWorker()
				t.Fatal("timed out waiting for standalone retry to start")
			}

			select {
			case result := <-resultCh:
				require.NoError(t, result.err)
				require.NotNil(t, result.info)
				assert.Equal(t, "retry:alpha", result.info.Name())
			case <-time.After(time.Second):
				releaseWorker()
				t.Fatal("timed out waiting for SubmitOne retry result")
			}

			snapshot := scheduler.Snapshot()
			assert.Equal(t, uint64(1), snapshot.Timeouts)
			assert.Equal(t, uint64(1), snapshot.Retries)
			assert.Equal(t, 1, snapshot.Workers)

			releaseWorker()
			require.Eventually(t, func() bool {
				snapshot := scheduler.Snapshot()
				return snapshot.StaleCompletions == 1 && snapshot.ActiveLeases == 0
			}, time.Second, 5*time.Millisecond)
		})
	}
}

func TestStatScheduler_SubmitOneStandaloneRetrySucceedsAfterLeaseTimeout(t *testing.T) {
	scheduler := newStatScheduler(nil, statSchedulerOptions{
		Workers:          1,
		MaxWorkers:       1,
		QueueDepth:       4,
		LeaseTimeout:     40 * time.Millisecond,
		RetryBackoff:     5 * time.Millisecond,
		WatchdogInterval: 5 * time.Millisecond,
		WarnAfter:        time.Second,
		ReplaceAfter:     60 * time.Millisecond,
	})
	defer scheduler.Close()

	firstStarted := make(chan struct{}, 1)
	retryStarted := make(chan struct{}, 1)
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	releaseWorker := func() {
		releaseOnce.Do(func() {
			close(releaseFirst)
		})
	}
	defer releaseWorker()
	var calls atomic.Int64
	scheduler.lstatFn = func(path string) (os.FileInfo, error) {
		call := calls.Add(1)
		if call == 1 {
			firstStarted <- struct{}{}
			<-releaseFirst
			return fakeFileInfo{name: "stale:" + path, mode: os.ModeSymlink}, nil
		}
		retryStarted <- struct{}{}
		return fakeFileInfo{name: "retry:" + path, mode: os.ModeSymlink}, nil
	}

	resultCh := make(chan struct {
		info os.FileInfo
		err  error
	}, 1)
	go func() {
		info, err := scheduler.SubmitOne(context.Background(), "alpha", true)
		resultCh <- struct {
			info os.FileInfo
			err  error
		}{info: info, err: err}
	}()

	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		releaseWorker()
		t.Fatal("timed out waiting for first standalone lstat to start")
	}

	select {
	case <-retryStarted:
	case <-time.After(time.Second):
		releaseWorker()
		t.Fatal("timed out waiting for standalone retry lstat to start")
	}

	select {
	case result := <-resultCh:
		require.NoError(t, result.err)
		require.NotNil(t, result.info)
		assert.Equal(t, "retry:alpha", result.info.Name())
		assert.Equal(t, os.ModeSymlink, result.info.Mode()&os.ModeSymlink)
	case <-time.After(time.Second):
		releaseWorker()
		t.Fatal("timed out waiting for SubmitOne retry success")
	}

	snapshot := scheduler.Snapshot()
	assert.Equal(t, uint64(1), snapshot.Timeouts)
	assert.Equal(t, uint64(1), snapshot.Retries)

	releaseWorker()
	require.Eventually(t, func() bool {
		snapshot := scheduler.Snapshot()
		return snapshot.StaleCompletions == 1 && snapshot.ActiveLeases == 0
	}, time.Second, 5*time.Millisecond)
}

func TestStatScheduler_SubmitOneLeaseTimeoutReclaimsWorker(t *testing.T) {
	scheduler := newStatScheduler(nil, statSchedulerOptions{
		Workers:          1,
		MaxWorkers:       1,
		QueueDepth:       4,
		LeaseTimeout:     40 * time.Millisecond,
		RetryBackoff:     5 * time.Millisecond,
		WatchdogInterval: 5 * time.Millisecond,
		WarnAfter:        time.Second,
		ReplaceAfter:     60 * time.Millisecond,
	})
	defer scheduler.Close()

	started := make(chan struct{}, 1)
	retryStarted := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseWorker := func() {
		releaseOnce.Do(func() {
			close(release)
		})
	}
	defer releaseWorker()
	var calls atomic.Int64
	scheduler.statFn = func(path string) (os.FileInfo, error) {
		call := calls.Add(1)
		if call == 1 {
			select {
			case started <- struct{}{}:
			default:
			}
			<-release
			return fakeFileInfo{name: "stale:" + path}, nil
		}
		select {
		case retryStarted <- struct{}{}:
		default:
		}
		return fakeFileInfo{name: "retry:" + path}, nil
	}

	resultCh := make(chan struct {
		info os.FileInfo
		err  error
	}, 1)
	go func() {
		info, err := scheduler.SubmitOne(context.Background(), "stuck", false)
		resultCh <- struct {
			info os.FileInfo
			err  error
		}{info: info, err: err}
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		releaseWorker()
		t.Fatal("timed out waiting for standalone stat to start")
	}

	select {
	case <-retryStarted:
	case <-time.After(time.Second):
		releaseWorker()
		t.Fatal("timed out waiting for standalone retry to start")
	}

	require.Eventually(t, func() bool {
		scheduler.mu.Lock()
		defer scheduler.mu.Unlock()
		return len(scheduler.workers) > 0 && scheduler.workers[0].generation >= 2 && scheduler.workers[0].accounted
	}, time.Second, 5*time.Millisecond)

	select {
	case result := <-resultCh:
		require.NoError(t, result.err)
		require.NotNil(t, result.info)
		assert.Equal(t, "retry:stuck", result.info.Name())
	case <-time.After(time.Second):
		releaseWorker()
		t.Fatal("timed out waiting for SubmitOne retry result")
	}

	releaseWorker()
	require.Eventually(t, func() bool {
		snapshot := scheduler.Snapshot()
		return snapshot.StaleCompletions == 1 && snapshot.ActiveLeases == 0
	}, time.Second, 5*time.Millisecond)
}

func TestStatScheduler_SubmitOneSchedulerShutdownDuringRetry(t *testing.T) {
	scheduler := newStatScheduler(nil, statSchedulerOptions{
		Workers:          1,
		MaxWorkers:       1,
		QueueDepth:       4,
		LeaseTimeout:     40 * time.Millisecond,
		RetryBackoff:     5 * time.Millisecond,
		WatchdogInterval: 5 * time.Millisecond,
		WarnAfter:        time.Second,
		ReplaceAfter:     60 * time.Millisecond,
	})

	firstStarted := make(chan struct{}, 1)
	retryStarted := make(chan struct{}, 1)
	releaseFirst := make(chan struct{})
	releaseRetry := make(chan struct{})
	var releaseFirstOnce sync.Once
	var releaseRetryOnce sync.Once
	releaseWorkers := func() {
		releaseFirstOnce.Do(func() {
			close(releaseFirst)
		})
		releaseRetryOnce.Do(func() {
			close(releaseRetry)
		})
	}
	defer releaseWorkers()
	var calls atomic.Int64
	scheduler.statFn = func(path string) (os.FileInfo, error) {
		call := calls.Add(1)
		if call == 1 {
			firstStarted <- struct{}{}
			<-releaseFirst
			return fakeFileInfo{name: "stale:" + path}, nil
		}
		retryStarted <- struct{}{}
		<-releaseRetry
		return fakeFileInfo{name: "retry:" + path}, nil
	}

	resultCh := make(chan error, 1)
	go func() {
		_, err := scheduler.SubmitOne(context.Background(), "shutdown", false)
		resultCh <- err
	}()

	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		releaseWorkers()
		scheduler.Close()
		t.Fatal("timed out waiting for first standalone stat to start")
	}

	select {
	case <-retryStarted:
	case <-time.After(time.Second):
		releaseWorkers()
		scheduler.Close()
		t.Fatal("timed out waiting for standalone retry to start")
	}

	closeDone := make(chan struct{})
	go func() {
		scheduler.Close()
		close(closeDone)
	}()

	select {
	case err := <-resultCh:
		require.Error(t, err)
		require.ErrorIs(t, err, context.Canceled)
		var pathErr *os.PathError
		require.ErrorAs(t, err, &pathErr)
		assert.Equal(t, "stat", pathErr.Op)
		assert.Equal(t, "shutdown", pathErr.Path)
		assert.False(t, os.IsNotExist(err))
	case <-time.After(time.Second):
		releaseWorkers()
		<-closeDone
		t.Fatal("timed out waiting for SubmitOne shutdown error")
	}

	releaseWorkers()
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("scheduler close did not finish after releasing retry workers")
	}
}

func TestStatScheduler_SubmitOneConcurrentCallers(t *testing.T) {
	scheduler := newStatScheduler(nil, statSchedulerOptions{
		Workers:          4,
		MaxWorkers:       4,
		QueueDepth:       16,
		LeaseTimeout:     time.Second,
		RetryBackoff:     time.Millisecond,
		WatchdogInterval: time.Second,
		WarnAfter:        time.Second,
		ReplaceAfter:     time.Second,
	})
	defer scheduler.Close()

	scheduler.statFn = func(path string) (os.FileInfo, error) {
		time.Sleep(5 * time.Millisecond)
		return fakeFileInfo{name: "stat:" + path}, nil
	}
	scheduler.lstatFn = func(path string) (os.FileInfo, error) {
		time.Sleep(5 * time.Millisecond)
		return fakeFileInfo{name: "lstat:" + path, mode: os.ModeSymlink}, nil
	}

	type want struct {
		path  string
		lstat bool
		name  string
	}
	wantResults := []want{
		{path: "alpha", name: "stat:alpha"},
		{path: "beta", lstat: true, name: "lstat:beta"},
		{path: "gamma", name: "stat:gamma"},
		{path: "delta", lstat: true, name: "lstat:delta"},
		{path: "epsilon", name: "stat:epsilon"},
	}

	got := make(chan string, len(wantResults))
	errCh := make(chan error, len(wantResults))
	var wg sync.WaitGroup
	for _, tc := range wantResults {
		wg.Add(1)
		go func(tc want) {
			defer wg.Done()
			info, err := scheduler.SubmitOne(context.Background(), tc.path, tc.lstat)
			if err != nil {
				errCh <- err
				return
			}
			got <- info.Name()
		}(tc)
	}
	wg.Wait()
	close(got)
	close(errCh)

	for err := range errCh {
		require.NoError(t, err)
	}

	var names []string
	for name := range got {
		names = append(names, name)
	}
	slices.Sort(names)

	var wantNames []string
	for _, tc := range wantResults {
		wantNames = append(wantNames, tc.name)
	}
	slices.Sort(wantNames)
	assert.Equal(t, wantNames, names)
}

func TestStatScheduler_SubmitOneReturnsWhenSchedulerCloses(t *testing.T) {
	scheduler := newStatScheduler(nil, statSchedulerOptions{
		Workers:          1,
		MaxWorkers:       1,
		QueueDepth:       4,
		LeaseTimeout:     time.Second,
		RetryBackoff:     time.Millisecond,
		WatchdogInterval: time.Second,
		WarnAfter:        time.Second,
		ReplaceAfter:     time.Second,
	})

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	scheduler.statFn = func(path string) (os.FileInfo, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return fakeFileInfo{name: path}, nil
	}

	resultCh := make(chan error, 1)
	go func() {
		_, err := scheduler.SubmitOne(context.Background(), "alpha", false)
		resultCh <- err
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		close(release)
		scheduler.Close()
		t.Fatal("timed out waiting for standalone stat to start")
	}

	closeDone := make(chan struct{})
	go func() {
		scheduler.Close()
		close(closeDone)
	}()

	select {
	case err := <-resultCh:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(250 * time.Millisecond):
		close(release)
		<-closeDone
		t.Fatal("SubmitOne did not return promptly after scheduler close")
	}

	close(release)
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("scheduler close did not finish after releasing blocked stat")
	}
}

func TestStatScheduler_CloseCancelsQueuedBatchJobs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	scheduler := &statScheduler{
		opts: statSchedulerOptions{
			MaxWorkers:   1,
			QueueDepth:   4,
			LeaseTimeout: time.Second,
		},
		ctx:         ctx,
		cancel:      cancel,
		normalQueue: make(chan *statJob, 4),
		retryQueue:  make(chan *statJob, 4),
		active:      make(map[statLeaseKey]*activeLease),
	}

	batch := newBatchController(context.Background(), []cachedDirEntry{
		{DirEntry: fakeDirEntry{name: "alpha"}},
	}, invalidStatDirFD, 1, func(entry *cachedDirEntry, nameBuf []byte) (os.FileInfo, []byte, error) {
		return fakeFileInfo{name: entry.Name()}, nameBuf, nil
	})
	defer batch.Close()

	require.NoError(t, batch.Schedule(scheduler))

	resultCh := make(chan error, 1)
	go func() {
		_, _, err := batch.Wait(context.Background())
		resultCh <- err
	}()

	scheduler.Close()

	select {
	case err := <-resultCh:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("queued batch did not unblock when scheduler closed")
	}

	batch.mu.Lock()
	assert.True(t, batch.canceled)
	require.ErrorIs(t, batch.firstErr, context.Canceled)
	batch.mu.Unlock()
}
