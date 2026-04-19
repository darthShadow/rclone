package local

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newLeaseTestScheduler(t *testing.T) *statScheduler {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	scheduler := &statScheduler{
		opts: statSchedulerOptions{
			LeaseTimeout:     time.Second,
			RetryBackoff:     time.Hour,
			WatchdogInterval: time.Second,
			WarnAfter:        2 * time.Second,
			ReplaceAfter:     3 * time.Second,
		},
		ctx:         ctx,
		cancel:      cancel,
		retryQueue:  make(chan *statJob, 1),
		workerCount: 1,
		workers: []statWorkerState{{
			generation: 1,
			accounted:  true,
		}},
		active:     make(map[statLeaseKey]*activeLease, 1),
		timeoutBuf: make([]leaseTimeoutEvent, 0, 1),
	}
	scheduler.wg.Add(1)
	return scheduler
}

func newLeaseTestBatch(t *testing.T) *batchController {
	t.Helper()

	batch := newBatchController(context.Background(), []cachedDirEntry{{DirEntry: fakeDirEntry{name: "alpha"}}}, invalidStatDirFD, 1, func(entry *cachedDirEntry, nameBuf []byte) (os.FileInfo, []byte, error) {
		return fakeFileInfo{name: entry.Name()}, nameBuf, nil
	})
	t.Cleanup(batch.Close)
	return batch
}

func TestStatScheduler_WatchdogDetectsTimeout(t *testing.T) {
	scheduler := newLeaseTestScheduler(t)
	batch := newLeaseTestBatch(t)
	job := scheduler.getJob()

	require.True(t, batch.acquirePendingLease(0, job))
	scheduler.beginLease(0, 1, job)

	events := scheduler.sweepLeases(time.Now().Add(time.Second))
	scheduler.cancel()
	scheduler.processEvents(events)

	assert.Equal(t, uint64(1), scheduler.Snapshot().Timeouts)

	batch.mu.Lock()
	assert.Equal(t, statMicroBatchRetryPending, batch.parts[0].state)
	assert.Equal(t, 1, batch.parts[0].retries)
	batch.mu.Unlock()
}

func TestStatScheduler_WatchdogDoesNotDoubleRetry(t *testing.T) {
	scheduler := newLeaseTestScheduler(t)
	batch := newLeaseTestBatch(t)
	job := scheduler.getJob()

	require.True(t, batch.acquirePendingLease(0, job))
	scheduler.beginLease(0, 1, job)

	events := scheduler.sweepLeases(time.Now().Add(time.Second))
	scheduler.cancel()
	scheduler.processEvents(events)

	events = scheduler.sweepLeases(time.Now().Add(2 * time.Second))
	scheduler.processEvents(events)

	assert.Equal(t, uint64(1), scheduler.Snapshot().Timeouts)

	batch.mu.Lock()
	assert.Equal(t, statMicroBatchRetryPending, batch.parts[0].state)
	assert.Equal(t, 1, batch.parts[0].retries)
	batch.mu.Unlock()
}

func TestStatScheduler_ReplaceBeforeLeaseTimeoutStillMarksLeaseTimedOut(t *testing.T) {
	scheduler := newLeaseTestScheduler(t)
	scheduler.opts.LeaseTimeout = 3 * time.Second
	scheduler.opts.ReplaceAfter = 200 * time.Millisecond
	batch := newLeaseTestBatch(t)
	job := scheduler.getJob()

	require.True(t, batch.acquirePendingLease(0, job))
	scheduler.beginLease(0, 1, job)

	events := scheduler.sweepLeases(time.Now().Add(250 * time.Millisecond))
	require.Len(t, events.timeouts, 1)
	require.Len(t, events.replaces, 1)

	scheduler.cancel()
	scheduler.processEvents(events)

	assert.Equal(t, uint64(1), scheduler.Snapshot().Timeouts)

	batch.closeDone()
	assert.False(t, batch.scratchReusable())

	batch.mu.Lock()
	assert.Equal(t, statMicroBatchRetryPending, batch.parts[0].state)
	assert.Equal(t, 1, batch.parts[0].retries)
	require.Len(t, batch.timedOutLeases, 1)
	_, ok := batch.timedOutLeases[batchLeaseKey{microBatch: 0, leaseID: 1}]
	assert.True(t, ok)
	batch.mu.Unlock()
}

func TestStatScheduler_FinishLeaseRetiresLateTimeoutWithoutLeak(t *testing.T) {
	scheduler := newLeaseTestScheduler(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	batch := newBatchController(ctx, []cachedDirEntry{{DirEntry: fakeDirEntry{name: "alpha"}}}, invalidStatDirFD, 1, func(entry *cachedDirEntry, nameBuf []byte) (os.FileInfo, []byte, error) {
		return fakeFileInfo{name: entry.Name()}, nameBuf, nil
	})
	t.Cleanup(batch.Close)
	job := scheduler.getJob()

	require.True(t, batch.acquirePendingLease(0, job))
	scheduler.beginLease(0, 1, job)

	events := scheduler.sweepLeases(time.Now().Add(time.Second))
	cancel()
	scheduler.finishLease(0, 1, job, false, false)

	reusedLease := scheduler.getLease()
	scheduler.putLease(reusedLease)
	reusedJob := scheduler.getJob()
	scheduler.putJob(reusedJob)

	scheduler.cancel()
	require.NotPanics(t, func() {
		scheduler.processEvents(events)
	})

	snapshot := scheduler.Snapshot()
	assert.Equal(t, 0, snapshot.ActiveLeases)
	assert.Equal(t, uint64(1), snapshot.StaleCompletions)
	assert.Equal(t, uint64(0), snapshot.Timeouts)

	batch.closeDone()
	assert.True(t, batch.scratchReusable())
	batch.mu.Lock()
	assert.Nil(t, batch.timedOutLeases)
	batch.mu.Unlock()
}

func TestStatScheduler_ProcessEventsCancelsWaitingBatchDuringShutdown(t *testing.T) {
	scheduler := newLeaseTestScheduler(t)
	batch := newLeaseTestBatch(t)
	job := scheduler.getJob()

	require.True(t, batch.acquirePendingLease(0, job))
	scheduler.beginLease(0, 1, job)

	resultCh := make(chan error, 1)
	go func() {
		_, _, err := batch.Wait(context.Background())
		resultCh <- err
	}()

	events := scheduler.sweepLeases(time.Now().Add(time.Second))
	scheduler.cancel()
	scheduler.processEvents(events)

	select {
	case err := <-resultCh:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for shutdown-time batch cancellation")
	}

	snapshot := scheduler.Snapshot()
	assert.Equal(t, uint64(1), snapshot.Timeouts)
	assert.Equal(t, uint64(0), snapshot.Retries)

	batch.mu.Lock()
	assert.True(t, batch.canceled)
	assert.Equal(t, statMicroBatchRetryPending, batch.parts[0].state)
	assert.Equal(t, 1, batch.parts[0].retries)
	assert.ErrorIs(t, batch.firstErr, context.Canceled)
	batch.mu.Unlock()
}
