package local

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rclone/rclone/fs"
)

const (
	statSchedulerDefaultWorkers        = 8
	statSchedulerDefaultMicroBatchSize = 8
	statSchedulerDefaultQueueDepth     = 256
	statSchedulerDefaultRetryBackoff   = 250 * time.Millisecond
	statSchedulerMaxRetryBackoff       = 5 * time.Minute
)

// statSchedulerOptions controls the per-Fs shared worker set, lease timeout,
// and retry/watchdog policy for scheduled stat microbatches.
type statSchedulerOptions struct {
	Workers          int
	MaxWorkers       int
	QueueDepth       int
	LeaseTimeout     time.Duration
	RetryBackoff     time.Duration
	WatchdogInterval time.Duration
	WarnAfter        time.Duration
	ReplaceAfter     time.Duration
}

func (o statSchedulerOptions) withDefaults() statSchedulerOptions {
	if o.Workers <= 0 {
		o.Workers = statSchedulerDefaultWorkers
	}
	if o.MaxWorkers < o.Workers {
		o.MaxWorkers = o.Workers * 2
	}
	if o.QueueDepth <= 0 {
		o.QueueDepth = statSchedulerDefaultQueueDepth
	}
	if o.LeaseTimeout <= 0 {
		o.LeaseTimeout = statTimeout * time.Second
	}
	if o.RetryBackoff <= 0 {
		o.RetryBackoff = statSchedulerDefaultRetryBackoff
	}
	if o.WatchdogInterval <= 0 {
		o.WatchdogInterval = watchdogInterval
	}
	if o.WarnAfter <= 0 {
		o.WarnAfter = stuckWarnTimeout
	}
	if o.ReplaceAfter <= 0 {
		o.ReplaceAfter = stuckReplaceTimeout
	}
	return o
}

// statSchedulerSnapshot exposes the current scheduler counters for tests and diagnostics.
type statSchedulerSnapshot struct {
	Workers          int
	ActiveLeases     int
	Timeouts         uint64
	Retries          uint64
	StaleCompletions uint64
}

type statLeaseKey struct {
	batch      *batchController
	microBatch int
	leaseID    uint64
}

type statWorkerState struct {
	currentJob *statJob
	startedAt  time.Time
	generation uint64
	accounted  bool
}

type statJobKind uint8

const (
	statJobBatch statJobKind = iota
	statJobStandalone
)

// statScheduler is the per-Fs worker pool that serializes stat, fstatat, and
// readdir-adjacent stat work behind lease timeouts, watchdog sweeps, and
// worker-replace semantics. Concurrent List calls on the same Fs share this
// scheduler; within a single listing the loop is sequential, so each ReadDir
// batch is fully statted and drained before the next begins.
//
// Two invariants are load-bearing here. First, neither the scheduler nor its
// callers may wait on a stuck CephFS syscall, whether directly, via channels
// that only close when leaked goroutines return, or via wg.Wait on workers that
// have already been accounted as stuck. Second, production stat work must stay
// inside the scheduler's own worker and timer lifecycle rather than starting
// separate goroutines that can block outside those bounds.
//
// SubmitOne is the single entry point for standalone stat and lstat calls.
// Batched listing stats flow through listController.ProcessBatch, which leases
// microbatches to long-lived workers and requeues timed-out work without
// publishing stale completions.
type statScheduler struct {
	owner *Fs
	opts  statSchedulerOptions

	ctx    context.Context
	cancel context.CancelFunc

	closing atomic.Bool

	normalQueue chan *statJob
	retryQueue  chan *statJob

	mu             sync.Mutex
	closed         bool
	wg             sync.WaitGroup
	jobPool        sync.Pool
	leasePool      sync.Pool
	nextWorker     int
	nextGeneration uint64
	freeWorkerIDs  []int
	workerCount    int
	workers        []statWorkerState
	statFn         func(string) (os.FileInfo, error)
	lstatFn        func(string) (os.FileInfo, error)
	active         map[statLeaseKey]*activeLease
	timeoutBuf     []leaseTimeoutEvent

	timeouts         atomic.Uint64
	retries          atomic.Uint64
	staleCompletions atomic.Uint64
}

// statJob is one leased microbatch from a filtered ReadDir batch. Workers
// compute into scratch slices and publish the whole job only if the lease is
// still current.
type statJob struct {
	kind       statJobKind
	batch      *batchController
	microBatch int
	start      int
	end        int
	leaseID    uint64
	path       string
	lstat      bool
}

type statLeaseTimeoutError struct {
	Path    string
	Lstat   bool
	Timeout time.Duration
}

func (e *statLeaseTimeoutError) Error() string {
	op := "stat"
	if e.Lstat {
		op = "lstat"
	}
	return fmt.Sprintf("%s %q timed out after %v", op, standalonePathLabel(e.Path), e.Timeout)
}

func (j *statJob) leaseKey() statLeaseKey {
	return statLeaseKey{
		batch:      j.batch,
		microBatch: j.microBatch,
		leaseID:    j.leaseID,
	}
}

func (j *statJob) label() string {
	if j == nil {
		return ""
	}
	if j.kind == statJobStandalone {
		return standalonePathLabel(j.path)
	}
	if j.batch == nil || j.start < 0 || j.start >= len(j.batch.entries) {
		return ""
	}
	return j.batch.entries[j.start].Name()
}

func (j *statJob) execute(s *statScheduler, resultsScratch []statFileInfo, nameBuf []byte) ([]statFileInfo, []byte, bool, bool) {
	if j.kind == statJobStandalone {
		return j.executeStandalone(s, resultsScratch, nameBuf)
	}
	return j.executeBatch(resultsScratch, nameBuf)
}

func (j *statJob) executeBatch(resultsScratch []statFileInfo, nameBuf []byte) ([]statFileInfo, []byte, bool, bool) {
	size := j.end - j.start
	if cap(resultsScratch) < size {
		resultsScratch = make([]statFileInfo, size)
	}
	results := resultsScratch[:size]
	var firstErr error

	for i := j.start; i < j.end; i++ {
		if err := j.batch.ctx.Err(); err != nil {
			return resultsScratch[:0], nameBuf, false, false
		}

		entry := &j.batch.entries[i]
		fi, nextNameBuf, err := runStatFunc(entry, j.batch.statFunc, nameBuf)
		nameBuf = nextNameBuf
		results[i-j.start] = fi
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	published, completed := j.batch.publish(j.microBatch, j.leaseID, results, firstErr)
	return resultsScratch[:0], nameBuf, published, completed
}

func (j *statJob) executeStandalone(s *statScheduler, resultsScratch []statFileInfo, nameBuf []byte) ([]statFileInfo, []byte, bool, bool) {
	if err := j.batch.ctx.Err(); err != nil {
		return resultsScratch[:0], nameBuf, false, false
	}
	if cap(resultsScratch) < 1 {
		resultsScratch = make([]statFileInfo, 1)
	}
	results := resultsScratch[:1]
	results[0] = statFileInfo{}

	fi, err := s.runStandaloneStat(j.path, j.lstat)
	results[0] = newStatFileInfoFromOS(fi)

	published, completed := j.batch.publish(j.microBatch, j.leaseID, results, err)
	return resultsScratch[:0], nameBuf, published, completed
}

// newStatScheduler creates the shared per-Fs stat scheduler with long-lived
// workers plus watchdog/retry control.
func newStatScheduler(owner *Fs, opts statSchedulerOptions) *statScheduler {
	opts = opts.withDefaults()

	ctx, cancel := context.WithCancel(context.Background())
	lstatFn := os.Lstat
	if owner != nil && owner.lstat != nil {
		lstatFn = owner.lstat
	}
	s := &statScheduler{
		owner:       owner,
		opts:        opts,
		ctx:         ctx,
		cancel:      cancel,
		normalQueue: make(chan *statJob, opts.QueueDepth),
		retryQueue:  make(chan *statJob, opts.QueueDepth),
		workers:     make([]statWorkerState, opts.MaxWorkers),
		statFn:      os.Stat,
		lstatFn:     lstatFn,
		active:      make(map[statLeaseKey]*activeLease, opts.MaxWorkers),
		timeoutBuf:  make([]leaseTimeoutEvent, 0, opts.MaxWorkers),
	}

	for i := 0; i < opts.Workers; i++ {
		_ = s.spawnWorker()
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.watchdog()
	}()

	return s
}

// Snapshot returns a point-in-time view of worker count, active leases, and cumulative event counters (timeouts, retries, stale completions).
func (s *statScheduler) Snapshot() statSchedulerSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	return statSchedulerSnapshot{
		Workers:          s.workerCount,
		ActiveLeases:     len(s.active),
		Timeouts:         s.timeouts.Load(),
		Retries:          s.retries.Load(),
		StaleCompletions: s.staleCompletions.Load(),
	}
}

// SubmitOne runs one stat or lstat call through the shared worker pool and
// waits for the result, caller cancellation, watchdog timeout, or scheduler
// shutdown. The effective timeout is min(caller deadline, LeaseTimeout). If the
// caller cancels, SubmitOne waits for the standalone lease to reach terminal
// state so no late result is returned after cancel, then returns ctx.Err().
//
// Single-path stat/lstat still routes through the scheduler so production code
// does not reintroduce uncapped stuck syscalls outside the shared lease/watchdog
// control path.
func (s *statScheduler) SubmitOne(ctx context.Context, path string, lstat bool) (os.FileInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}

	submitCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	controller := newStandaloneBatchController(submitCtx, path, lstat)
	defer controller.Close()

	job := s.getJob()
	ok := controller.acquirePendingLease(0, job)
	if !ok {
		s.putJob(job)
		return nil, context.Canceled
	}
	if err := s.enqueueNormal(submitCtx, job); err != nil {
		controller.revertIssuedLease(job.microBatch, job.leaseID, false)
		s.putJob(job)
		return nil, err
	}

	timeoutCh := controller.timeoutNotifications()
	terminalCh := controller.terminalNotifications()
	for {
		select {
		case <-controller.done:
			if err := ctx.Err(); err != nil {
				return nil, s.waitStandaloneCancel(controller, terminalCh, err)
			}
			if err := s.ctx.Err(); err != nil {
				controller.cancel()
				return nil, err
			}
			_, fis, err := controller.Results()
			if err != nil {
				return nil, err
			}
			if len(fis) == 0 {
				return nil, fmt.Errorf("standalone stat completed without a result slot")
			}
			if fis[0].fi == nil {
				return nil, fmt.Errorf("standalone stat completed without file info")
			}
			return fis[0].fi, nil
		case err := <-timeoutCh:
			if cancelErr := ctx.Err(); cancelErr != nil {
				return nil, s.waitStandaloneCancel(controller, terminalCh, cancelErr)
			}
			if schedulerErr := s.ctx.Err(); schedulerErr != nil {
				controller.cancel()
				return nil, schedulerErr
			}
			controller.cancel()
			return nil, err
		case <-ctx.Done():
			return nil, s.waitStandaloneCancel(controller, terminalCh, ctx.Err())
		case <-s.ctx.Done():
			controller.cancel()
			return nil, s.ctx.Err()
		}
	}
}

func (s *statScheduler) waitStandaloneCancel(controller *batchController, terminalCh <-chan struct{}, err error) error {
	controller.cancel()

	for {
		select {
		case <-terminalCh:
			return err
		case <-s.ctx.Done():
			return s.ctx.Err()
		}
	}
}

// Close stops the scheduler and waits briefly for non-stuck workers to exit.
func (s *statScheduler) Close() {
	s.closing.Store(true)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.cancel()
	s.mu.Unlock()

	done := make(chan struct{})
	// If the timeout path fires, this goroutine stays blocked in Wait until the
	// stuck workers eventually return or the process exits.
	go func() {
		s.wg.Wait()
		close(done)
	}()

	// Keep the shutdown fallback at or above LeaseTimeout. Once it fires, every
	// active lease is old enough for sweepLeases(time.Now()) to emit timeout
	// events, so batchController.Wait callers unblock via lease-cancel even if
	// the watchdog ticker has already stopped.
	closeFallback := 30 * time.Second
	if s.opts.LeaseTimeout > closeFallback {
		closeFallback = s.opts.LeaseTimeout
	}

	select {
	case <-done:
	case <-time.After(closeFallback):
		fs.Errorf(s.owner, "stat scheduler: %d workers still running after close", s.Snapshot().Workers)
		events := s.sweepLeases(time.Now())
		s.processEvents(events)
	}

	s.drainQueuedJobs(s.normalQueue)
	s.drainQueuedJobs(s.retryQueue)
}

func (s *statScheduler) spawnWorker() bool {
	s.mu.Lock()
	if s.closed || s.ctx.Err() != nil || s.workerCount >= s.opts.MaxWorkers {
		s.mu.Unlock()
		return false
	}

	workerID := 0
	if n := len(s.freeWorkerIDs); n > 0 {
		workerID = s.freeWorkerIDs[n-1]
		s.freeWorkerIDs = s.freeWorkerIDs[:n-1]
	} else {
		workerID = s.nextWorker
		if workerID >= len(s.workers) {
			s.mu.Unlock()
			return false
		}
		s.nextWorker++
	}
	s.nextGeneration++
	workerGeneration := s.nextGeneration
	s.workerCount++
	s.workers[workerID] = statWorkerState{
		generation: workerGeneration,
		accounted:  true,
	}
	s.wg.Add(1)
	s.mu.Unlock()

	go s.workerLoop(workerID, workerGeneration)
	return true
}

func (s *statScheduler) workerLoop(workerID int, workerGeneration uint64) {
	defer s.finishWorker(workerID, workerGeneration)

	var resultsScratch []statFileInfo
	nameBuf := make([]byte, 0, 256)

	for {
		job, ok := s.nextJob()
		if !ok {
			return
		}
		if s.closing.Load() {
			s.failQueuedJob(job, s.schedulerClosedErr())
			continue
		}
		if job.kind == statJobStandalone && job.batch.standaloneCanceled() {
			// A standalone job can be canceled while still queued; close its
			// terminal path without beginning a lease or entering a syscall.
			s.finishLease(workerID, workerGeneration, job, false, false)
			continue
		}

		var published, completed bool
		var panicValue any
		resultsScratch, nameBuf, published, completed, panicValue = s.executeLease(workerID, workerGeneration, job, resultsScratch, nameBuf)
		var panicTarget any
		if panicValue != nil {
			// Snapshot the final panic log target before finishLease returns the
			// pooled job to putJob; otherwise a subsequent fillJobLocked can race
			// this read by reusing the same object.
			panicTarget = s.leaseLogTarget(job.batch, job.label())
		}
		reclaimed := s.finishLease(workerID, workerGeneration, job, published, completed)
		if panicValue != nil {
			fs.Errorf(panicTarget, "stat scheduler worker panic: %v", panicValue)
		}
		if reclaimed {
			return
		}
	}
}

func (s *statScheduler) finishWorker(workerID int, workerGeneration uint64) {
	accounted := false

	s.mu.Lock()
	if workerID >= 0 && workerID < len(s.workers) {
		worker := &s.workers[workerID]
		if worker.generation == workerGeneration && worker.accounted {
			s.workers[workerID] = statWorkerState{}
			s.freeWorkerIDs = append(s.freeWorkerIDs, workerID)
			s.workerCount--
			accounted = true
		}
	}
	s.mu.Unlock()

	if accounted {
		s.wg.Done()
	}
}

// executeLease is the outer worker panic-recovery boundary around one leased
// job. It converts panics into panicValue so workerLoop can still call
// finishLease and retire scheduler/batch lease state before logging.
func (s *statScheduler) executeLease(workerID int, workerGeneration uint64, job *statJob, resultsScratch []statFileInfo, nameBuf []byte) (nextResultsScratch []statFileInfo, nextNameBuf []byte, published bool, completed bool, panicValue any) {
	nextResultsScratch = resultsScratch
	nextNameBuf = nameBuf

	defer func() {
		panicValue = recover()
		if panicValue == nil {
			return
		}
		published, completed = job.batch.publish(job.microBatch, job.leaseID, nil, statSchedulerPanicError(job, panicValue))
	}()

	s.beginLease(workerID, workerGeneration, job)
	nextResultsScratch, nextNameBuf, published, completed = job.execute(s, resultsScratch, nameBuf)
	return nextResultsScratch, nextNameBuf, published, completed, nil
}

func statSchedulerRetryBackoff(base time.Duration, retries int) time.Duration {
	if base <= 0 {
		base = statSchedulerDefaultRetryBackoff
	}
	if base >= statSchedulerMaxRetryBackoff {
		return statSchedulerMaxRetryBackoff
	}
	if retries <= 1 {
		return base
	}

	backoff := base
	for attempt := 1; attempt < retries; attempt++ {
		if backoff >= statSchedulerMaxRetryBackoff/2 {
			return statSchedulerMaxRetryBackoff
		}
		backoff *= 2
	}
	if backoff > statSchedulerMaxRetryBackoff {
		return statSchedulerMaxRetryBackoff
	}
	return backoff
}

func statSchedulerPanicError(job *statJob, panicValue any) error {
	label := job.label()
	if recoveredErr, ok := panicValue.(error); ok {
		if label != "" {
			return fmt.Errorf("stat scheduler panic for %q [%d:%d): %w", label, job.start, job.end, recoveredErr)
		}
		return fmt.Errorf("stat scheduler panic [%d:%d): %w", job.start, job.end, recoveredErr)
	}
	if label != "" {
		return fmt.Errorf("stat scheduler panic for %q [%d:%d): %v", label, job.start, job.end, panicValue)
	}
	return fmt.Errorf("stat scheduler panic [%d:%d): %v", job.start, job.end, panicValue)
}

func standalonePathLabel(path string) string {
	if path == "" {
		return ""
	}
	label := filepath.Base(path)
	if label == "" || label == "." || label == string(filepath.Separator) {
		return path
	}
	return label
}

func (s *statScheduler) runStandaloneStat(path string, lstat bool) (os.FileInfo, error) {
	if lstat {
		if s.owner != nil && s.owner.lstat != nil {
			return s.owner.lstat(path)
		}
		return s.lstatFn(path)
	}
	return s.statFn(path)
}

func (s *statScheduler) nextJob() (*statJob, bool) {
	select {
	case <-s.ctx.Done():
		return nil, false
	case job := <-s.normalQueue:
		return job, true
	case job := <-s.retryQueue:
		return job, true
	}
}

func (s *statScheduler) enqueueNormal(ctx context.Context, job *statJob) error {
	if s.closing.Load() {
		return s.schedulerClosedErr()
	}
	select {
	case <-s.ctx.Done():
		return s.ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	case s.normalQueue <- job:
		return nil
	}
}

func (s *statScheduler) enqueueRetry(ctx context.Context, job *statJob) error {
	if s.closing.Load() {
		return s.schedulerClosedErr()
	}
	select {
	case <-s.ctx.Done():
		return s.ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	case s.retryQueue <- job:
		return nil
	}
}

func (s *statScheduler) logTargetLabel(label string) any {
	if label != "" {
		return label
	}
	return s.owner
}

func (s *statScheduler) getJob() *statJob {
	if pooled := s.jobPool.Get(); pooled != nil {
		return pooled.(*statJob)
	}
	return &statJob{}
}

func (s *statScheduler) putJob(job *statJob) {
	*job = statJob{}
	s.jobPool.Put(job)
}

func (s *statScheduler) schedulerClosedErr() error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	return context.Canceled
}

func (s *statScheduler) failQueuedJob(job *statJob, err error) {
	if job == nil {
		return
	}
	if err == nil {
		err = context.Canceled
	}
	if job.batch != nil {
		job.batch.mu.Lock()
		if job.batch.firstErr == nil {
			job.batch.firstErr = err
		}
		job.batch.canceled = true
		job.batch.mu.Unlock()
		job.batch.closeDone()
		job.batch.signalTerminal()
	}
	s.putJob(job)
}

func (s *statScheduler) drainQueuedJobs(queue chan *statJob) {
	for {
		select {
		case job := <-queue:
			s.failQueuedJob(job, s.schedulerClosedErr())
		default:
			return
		}
	}
}
