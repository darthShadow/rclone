package local

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"
)

type statMicroBatchState uint8

const (
	statMicroBatchPending statMicroBatchState = iota
	statMicroBatchLeased
	statMicroBatchRetryPending
	statMicroBatchDone
)

// listControllerOptions controls per-list microbatch sizing.
type listControllerOptions struct {
	MicroBatchSize int
}

func (o listControllerOptions) withDefaults() listControllerOptions {
	if o.MicroBatchSize <= 0 {
		o.MicroBatchSize = statSchedulerDefaultMicroBatchSize
	}
	return o
}

type batchPart struct {
	start          int
	end            int
	state          statMicroBatchState
	leaseID        uint64
	retiredLeaseID uint64
	retries        int
}

type batchLeaseKey struct {
	microBatch int
	leaseID    uint64
}

// batchController owns either one filtered ReadDir batch with its batch-scoped
// stat fd, lease state, and indexed result slots, or one standalone stat job
// submitted through SubmitOne with a single result slot.
type batchController struct {
	ctx      context.Context
	entries  []cachedDirEntry
	statDir  int
	statFunc func(*cachedDirEntry, []byte) (os.FileInfo, []byte, error)
	// standalone marks the SubmitOne tagged-union variant. Immutable after construction.
	standalone bool
	// standalonePath is the SubmitOne path. Immutable after construction.
	standalonePath string
	// standaloneLstat selects lstat vs stat for SubmitOne. Immutable after construction.
	standaloneLstat bool
	results         []statFileInfo
	parts           []batchPart
	remainingParts  int
	done            chan struct{}

	mu          sync.Mutex
	canceled    bool
	doneClosed  bool
	firstErr    error
	closeOnce   sync.Once
	doneOnce    sync.Once
	timeoutOnce sync.Once

	// timedOutLeases tracks stale timed-out workers that are still in flight.
	// Mutate it only under b.mu. scratchReusable consults this set before
	// handing batch scratch back to the listing controller, but done closes when
	// the result slots are published even if stale workers are still reading
	// entries.
	timedOutLeases map[batchLeaseKey]struct{}
	// timeoutCh is allocated only by newStandaloneBatchController. The watchdog
	// timeout path delivers the first standalone timeout error on it; batched
	// controllers leave it nil so stray notifications no-op.
	timeoutCh chan error
}

type statFileInfo struct {
	fi os.FileInfo
}

func (fi statFileInfo) Name() string {
	if fi.fi == nil {
		return ""
	}
	return fi.fi.Name()
}

func newStatFileInfoFromOS(info os.FileInfo) statFileInfo {
	if info == nil {
		return statFileInfo{}
	}
	return statFileInfo{fi: info}
}

type standaloneDirEntry struct {
	name string
}

func (e standaloneDirEntry) Name() string      { return e.name }
func (e standaloneDirEntry) IsDir() bool       { return false }
func (e standaloneDirEntry) Type() os.FileMode { return 0 }
func (e standaloneDirEntry) Info() (os.FileInfo, error) {
	return nil, fmt.Errorf("standalone stat entry %q has no inline info", e.name)
}

func newStandaloneBatchController(ctx context.Context, path string, lstat bool) *batchController {
	entry := cachedDirEntry{DirEntry: standaloneDirEntry{name: path}}
	b := newBatchController(ctx, []cachedDirEntry{entry}, invalidStatDirFD, 1, nil)
	b.standalone = true
	b.standalonePath = path
	b.standaloneLstat = lstat
	// Standalone controllers always allocate the timeout notification channel.
	b.timeoutCh = make(chan error, 1)
	return b
}

// listController owns one directory listing. It processes ReadDir batches
// sequentially and submits every filtered batch to the shared scheduler.
type listController struct {
	owner     *Fs
	scheduler *statScheduler
	opts      listControllerOptions
	dirPath   string

	entriesScratch []cachedDirEntry
	resultsScratch []statFileInfo
	partsScratch   []batchPart
}

// newBatchController constructs the controller for one filtered ReadDir batch
// and takes ownership of that batch's stat fd.
func newBatchController(ctx context.Context, entries []cachedDirEntry, statDir int, microBatchSize int, statFunc func(*cachedDirEntry, []byte) (os.FileInfo, []byte, error)) *batchController {
	return newBatchControllerWithScratch(ctx, entries, statDir, microBatchSize, statFunc, nil, nil)
}

func newBatchControllerWithScratch(ctx context.Context, entries []cachedDirEntry, statDir int, microBatchSize int, statFunc func(*cachedDirEntry, []byte) (os.FileInfo, []byte, error), resultsScratch []statFileInfo, partsScratch []batchPart) *batchController {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = context.WithoutCancel(ctx)
	if microBatchSize <= 0 {
		microBatchSize = statSchedulerDefaultMicroBatchSize
	}

	partCount := 0
	if len(entries) > 0 {
		partCount = (len(entries) + microBatchSize - 1) / microBatchSize
	}
	var results []statFileInfo
	if cap(resultsScratch) >= len(entries) {
		results = resultsScratch[:len(entries)]
		clear(results)
	} else {
		results = make([]statFileInfo, len(entries))
	}
	var parts []batchPart
	if cap(partsScratch) >= partCount {
		parts = partsScratch[:partCount]
	} else {
		parts = make([]batchPart, partCount)
	}
	partIndex := 0
	for start := 0; start < len(entries); start += microBatchSize {
		end := start + microBatchSize
		if end > len(entries) {
			end = len(entries)
		}
		parts[partIndex] = batchPart{
			start: start,
			end:   end,
			state: statMicroBatchPending,
		}
		partIndex++
	}

	b := &batchController{
		ctx:            ctx,
		entries:        entries,
		statDir:        statDir,
		statFunc:       statFunc,
		results:        results,
		parts:          parts,
		remainingParts: len(parts),
		done:           make(chan struct{}),
	}
	if len(parts) == 0 {
		b.closeDone()
	}
	return b
}

func (b *batchController) closeDone() {
	b.doneOnce.Do(func() {
		b.mu.Lock()
		b.doneClosed = true
		b.mu.Unlock()
		close(b.done)
	})
}

// Close releases the batch-scoped stat fd. The raw dirfd is snapshotted into each cachedDirEntry during ProcessBatch; after Close, stale workers still holding that integer receive EBADF, but their results are discarded via lease validation in publish. Idempotent via closeOnce.
func (b *batchController) Close() {
	b.closeOnce.Do(func() {
		closeStatDirFD(&b.statDir)
	})
}

// Schedule splits the batch into leased microbatches and submits them to the
// shared scheduler.
func (b *batchController) Schedule(scheduler *statScheduler) error {
	if scheduler == nil {
		return fmt.Errorf("nil stat scheduler: %w", errStatSchedulerBug)
	}

	for microBatch := range b.parts {
		job := scheduler.getJob()
		ok := b.acquirePendingLease(microBatch, job)
		if !ok {
			scheduler.putJob(job)
			continue
		}
		if err := scheduler.enqueueNormal(b.ctx, job); err != nil {
			b.revertIssuedLease(job.microBatch, job.leaseID, false)
			scheduler.putJob(job)
			b.cancel()
			return err
		}
	}
	return nil
}

// Wait blocks until the whole batch is finished or the listing context is canceled.
func (b *batchController) Wait(ctx context.Context) ([]cachedDirEntry, []statFileInfo, error) {
	select {
	case <-b.done:
		return b.Results()
	case <-ctx.Done():
		b.cancel()
		return nil, nil, ctx.Err()
	}
}

// Results returns the batch-owned entry and result slices without compaction.
// Result slots whose fi is nil must be skipped by the caller. Entry slots stay
// at stable indexes for the lifetime of the batch, and callers must not write
// to either returned slice.
func (b *batchController) Results() ([]cachedDirEntry, []statFileInfo, error) {
	return b.entries, b.results, b.firstErr
}

func runStatFunc(entry *cachedDirEntry, statFunc func(*cachedDirEntry, []byte) (os.FileInfo, []byte, error), nameBuf []byte) (result statFileInfo, nextNameBuf []byte, err error) {
	nextNameBuf = nameBuf
	defer func() {
		if r := recover(); r != nil {
			if recoveredErr, ok := r.(error); ok {
				err = fmt.Errorf("stat panic for %q: %w: %w", entry.Name(), errStatSchedulerBug, recoveredErr)
			} else {
				err = fmt.Errorf("stat panic for %q: %w: %v", entry.Name(), errStatSchedulerBug, r)
			}
		}
	}()
	result.fi, nextNameBuf, err = statFunc(entry, nameBuf)
	return result, nextNameBuf, err
}

func (b *batchController) acquirePendingLease(microBatch int, job *statJob) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	part := &b.parts[microBatch]
	if b.canceled || part.state != statMicroBatchPending {
		return false
	}

	part.state = statMicroBatchLeased
	part.leaseID++
	b.fillJobLocked(microBatch, job)
	return true
}

func (b *batchController) acquireRetryLease(microBatch int, job *statJob) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	part := &b.parts[microBatch]
	if b.canceled || part.state != statMicroBatchRetryPending {
		return false
	}

	part.state = statMicroBatchLeased
	part.leaseID++
	b.fillJobLocked(microBatch, job)
	return true
}

func (b *batchController) markLeaseTimedOut(microBatch int, leaseID uint64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	part := &b.parts[microBatch]
	if part.retiredLeaseID == leaseID || b.canceled || part.state != statMicroBatchLeased || part.leaseID != leaseID {
		return false
	}

	part.retries++
	part.state = statMicroBatchRetryPending
	if b.timedOutLeases == nil {
		b.timedOutLeases = make(map[batchLeaseKey]struct{})
	}
	b.timedOutLeases[batchLeaseKey{microBatch: microBatch, leaseID: leaseID}] = struct{}{}
	return true
}

func (b *batchController) retryBackoff(microBatch int, base time.Duration) time.Duration {
	b.mu.Lock()
	retries := b.parts[microBatch].retries
	b.mu.Unlock()

	return statSchedulerRetryBackoff(base, retries)
}

func (b *batchController) revertIssuedLease(microBatch int, leaseID uint64, retry bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	part := &b.parts[microBatch]
	if part.state != statMicroBatchLeased || part.leaseID != leaseID {
		return
	}
	if retry || part.retries > 0 {
		part.state = statMicroBatchRetryPending
		return
	}
	part.state = statMicroBatchPending
}

func (b *batchController) publish(microBatch int, leaseID uint64, results []statFileInfo, firstErr error) (bool, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	part := &b.parts[microBatch]
	// Timed-out/retried leases can complete after ProcessBatch returns and
	// Close recycles the batch dirfd; those stale workers still hold the raw fd
	// integer and observe EBADF. This publish gate is also the correctness check
	// that rejects any late worker after its lease times out or the batch is
	// canceled. markLeaseTimedOut flips part.state to retry-pending under b.mu,
	// and this publish path reacquires b.mu before accepting results, so stale
	// completions fail the canceled/state/leaseID gate and are discarded instead
	// of overwriting fresh work.
	if b.canceled || part.state != statMicroBatchLeased || part.leaseID != leaseID {
		return false, false
	}

	copy(b.results[part.start:part.end], results)
	if b.firstErr == nil && firstErr != nil {
		b.firstErr = firstErr
	}
	part.state = statMicroBatchDone
	b.remainingParts--
	return true, b.remainingParts == 0
}

// retireLease records lease retirement after finishLease removes scheduler
// state. It only accepts leases that either timed out or published the final
// batch result part. A completed lease closes done as soon as the batch's
// result slots are published; stale timed-out workers may still be reading
// entries after done closes because entries are pre-populated before any lease
// is issued and workers only observe them afterward. Late timeout events are
// rejected by
// recording retiredLeaseID before processEvents can re-mark the lease as timed
// out.
func (b *batchController) retireLease(microBatch int, leaseID uint64, timedOut bool, published bool, completed bool) {
	if !timedOut && !completed {
		return
	}

	b.mu.Lock()
	if timedOut {
		part := &b.parts[microBatch]
		if !published {
			// Reject late timeout events after a timed-out lease has already retired.
			part.retiredLeaseID = leaseID
		}
		if b.timedOutLeases != nil {
			delete(b.timedOutLeases, batchLeaseKey{microBatch: microBatch, leaseID: leaseID})
			if len(b.timedOutLeases) == 0 {
				b.timedOutLeases = nil
			}
		}
	}
	b.mu.Unlock()

	if completed {
		b.closeDone()
	}
}

// scratchReusable reports whether a completed batch can hand scratch back to the
// listing controller. The doneClosed and timedOutLeases gate prevents reuse while
// stale timed-out workers may still alias entry or result scratch.
func (b *batchController) scratchReusable() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.doneClosed && !b.canceled && len(b.timedOutLeases) == 0
}

func (b *batchController) cancel() {
	b.mu.Lock()
	b.canceled = true
	b.mu.Unlock()
}

func (b *batchController) cancelWithErr(err error) {
	b.mu.Lock()
	if b.firstErr == nil {
		b.firstErr = err
	}
	b.canceled = true
	b.mu.Unlock()
	b.closeDone()
}

func (b *batchController) timeoutNotifications() <-chan error {
	return b.timeoutCh
}

// notifyTimeout publishes the first standalone timeout error. Batched
// controllers keep timeoutCh nil, so accidental calls are harmless.
func (b *batchController) notifyTimeout(err error) {
	if b.timeoutCh == nil || err == nil {
		return
	}
	b.timeoutOnce.Do(func() {
		b.timeoutCh <- err
	})
}

func (b *batchController) standaloneCanceled() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.standalone && b.canceled
}

func (b *batchController) fillJobLocked(microBatch int, job *statJob) {
	part := b.parts[microBatch]
	*job = statJob{
		kind:       statJobBatch,
		batch:      b,
		microBatch: microBatch,
		start:      part.start,
		end:        part.end,
		leaseID:    part.leaseID,
	}
	if !b.standalone {
		return
	}
	job.kind = statJobStandalone
	job.path = b.standalonePath
	job.lstat = b.standaloneLstat
}

// newListController constructs one listing-scoped batch coordinator for the
// sequential ReadDir -> ProcessBatch -> materialize loop.
func newListController(owner *Fs, scheduler *statScheduler, opts listControllerOptions) *listController {
	opts = opts.withDefaults()
	return &listController{
		owner:     owner,
		scheduler: scheduler,
		opts:      opts,
	}
}

// reclaimScratch silently leaves scratch with the batch when scratchReusable is
// false. Timed-out stale workers may still alias that storage, so the next batch
// must allocate or use different scratch instead of reusing it.
func (l *listController) reclaimScratch(controller *batchController) {
	if controller == nil || !controller.scratchReusable() {
		return
	}
	l.entriesScratch = controller.entries[:0]
	l.resultsScratch = controller.results[:0]
	l.partsScratch = controller.parts[:0]
}

// ProcessBatch filters one ReadDir batch, snapshots any batch-scoped dirfd,
// submits the filtered entries to the shared scheduler, and waits for that batch to finish
// before the caller advances to the next ReadDir batch.
func (l *listController) ProcessBatch(ctx context.Context, batch *readResult, preFilter func(os.DirEntry) (cachedDirEntry, bool), statFunc func(*cachedDirEntry, []byte) (os.FileInfo, []byte, error), consume listBatchConsumer) error {
	// Mirror SubmitOne's nil handling: ProcessBatch is a public scheduler entry,
	// and context.WithoutCancel(nil) would panic.
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = context.WithoutCancel(ctx)
	path := l.dirPath

	var controller *batchController
	defer func() {
		if controller != nil {
			controller.Close()
			return
		}
		closeReadResultStatDir(batch)
	}()

	if len(batch.entries) == 0 {
		return nil
	}

	// Snapshot the raw dirfd once for this batch so each entry can reuse it.
	statDirFD, useStatFD := listStatDirFD(batch.statDir)
	entriesScratch := l.entriesScratch
	l.entriesScratch = nil
	entries := entriesScratch[:0]
	if cap(entries) < len(batch.entries) {
		entries = make([]cachedDirEntry, 0, len(batch.entries))
	}

	if preFilter != nil {
		for _, entry := range batch.entries {
			filteredEntry, ok := preFilter(entry)
			if !ok {
				continue
			}
			filteredEntry.statDirFD = statDirFD
			filteredEntry.useStatFD = useStatFD
			entries = append(entries, filteredEntry)
		}
	} else {
		for _, entry := range batch.entries {
			entries = append(entries, cachedDirEntry{
				DirEntry:  entry,
				statDirFD: statDirFD,
				useStatFD: useStatFD,
			})
		}
	}

	if len(entries) == 0 {
		l.entriesScratch = entries[:0]
		return nil
	}

	// Scratch ownership moves to the batch until reclaimScratch hands it back.
	resultsScratch := l.resultsScratch
	partsScratch := l.partsScratch
	l.resultsScratch = nil
	l.partsScratch = nil
	controller = newBatchControllerWithScratch(ctx, entries, batch.statDir, l.opts.MicroBatchSize, statFunc, resultsScratch, partsScratch)
	batch.statDir = invalidStatDirFD

	err := controller.Schedule(l.scheduler)
	if err != nil {
		l.reclaimScratch(controller)
		return translateSchedulerErr("list", path, err)
	}
	batchEntries, fis, err := controller.Wait(ctx)
	if err != nil {
		l.reclaimScratch(controller)
		return translateSchedulerErr("list", path, err)
	}
	if consume != nil {
		err = consume(batchEntries, fis)
	}
	l.reclaimScratch(controller)
	return translateSchedulerErr("list", path, err)
}
