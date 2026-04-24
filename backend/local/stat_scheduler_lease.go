package local

import (
	"time"

	"github.com/rclone/rclone/fs"
)

type activeLease struct {
	batch      *batchController
	microBatch int
	leaseID    uint64
	start      int
	end        int
	label      string
	workerID   int
	started    time.Time
	timedOut   bool
	warned     bool
	replaced   bool

	workerGeneration uint64
	workerReclaimed  bool
}

type leaseTimeoutEvent struct {
	batch      *batchController
	microBatch int
	leaseID    uint64
	label      string
	start      int
	end        int
}

type leaseEvents struct {
	timeouts []leaseTimeoutEvent
	warns    []warnEvent
	replaces []replaceEvent
}

type warnEvent struct {
	batch *batchController
	label string
	start int
	end   int
	age   time.Duration
}

type replaceEvent struct {
	batch      *batchController
	microBatch int
	leaseID    uint64
	label      string
	start      int
	end        int
}

var newStatSchedulerRetryTimer = time.NewTimer

func (s *statScheduler) getLease() *activeLease {
	if pooled := s.leasePool.Get(); pooled != nil {
		return pooled.(*activeLease)
	}
	return &activeLease{}
}

func (s *statScheduler) putLease(lease *activeLease) {
	*lease = activeLease{}
	s.leasePool.Put(lease)
}

func (s *statScheduler) beginLease(workerID int, workerGeneration uint64, job *statJob) {
	key := job.leaseKey()
	now := time.Now()

	lease := s.getLease()
	lease.batch = job.batch
	lease.microBatch = job.microBatch
	lease.leaseID = job.leaseID
	lease.start = job.start
	lease.end = job.end
	lease.workerID = workerID
	lease.workerGeneration = workerGeneration
	lease.started = now
	lease.label = job.label()

	s.mu.Lock()
	if workerID >= 0 && workerID < len(s.workers) {
		worker := &s.workers[workerID]
		if worker.generation == workerGeneration && worker.accounted {
			worker.currentJob = job
		}
	}
	s.active[key] = lease
	s.mu.Unlock()
}

func (s *statScheduler) finishLease(workerID int, workerGeneration uint64, job *statJob, published bool, completed bool) bool {
	key := job.leaseKey()
	var lease *activeLease
	timedOut := false
	reclaimed := false

	s.mu.Lock()
	lease = s.active[key]
	if lease != nil {
		delete(s.active, key)
		timedOut = lease.timedOut
		reclaimed = lease.workerReclaimed
	}
	if workerID >= 0 && workerID < len(s.workers) {
		worker := &s.workers[workerID]
		if worker.generation == workerGeneration && worker.accounted && worker.currentJob == job {
			worker.currentJob = nil
		}
	}
	s.mu.Unlock()

	// Standalone terminal notification must happen after retireLease records the
	// lease outcome; retireLease must run before the pooled job and lease state
	// are released.
	job.batch.retireLease(job.microBatch, job.leaseID, timedOut, published, completed)
	if lease != nil {
		s.putLease(lease)
	}
	if !published {
		s.staleCompletions.Add(1)
	}
	s.putJob(job)
	return reclaimed
}

func (s *statScheduler) watchdogTickInterval() time.Duration {
	interval := s.opts.WatchdogInterval
	if s.opts.LeaseTimeout < interval {
		interval = s.opts.LeaseTimeout
	}
	return interval
}

func (s *statScheduler) sweepLeases(now time.Time) leaseEvents {
	s.mu.Lock()
	// The returned timeouts slice aliases s.timeoutBuf. Callers must consume it
	// before the next sweepLeases on the same scheduler, which reuses that buffer.
	timeouts := s.timeoutBuf[:0]
	var warns []warnEvent
	var replaces []replaceEvent
	for _, lease := range s.active {
		age := now.Sub(lease.started)

		if age >= s.opts.LeaseTimeout && !lease.timedOut {
			lease.timedOut = true
			timeouts = append(timeouts, leaseTimeoutEvent{
				batch:      lease.batch,
				microBatch: lease.microBatch,
				leaseID:    lease.leaseID,
				label:      lease.label,
				start:      lease.start,
				end:        lease.end,
			})
		}
		if age >= s.opts.WarnAfter && !lease.warned {
			lease.warned = true
			warns = append(warns, warnEvent{
				batch: lease.batch,
				label: lease.label,
				start: lease.start,
				end:   lease.end,
				age:   age,
			})
		}
		// ReplaceAfter must force the timeout transition first so cancel/retry
		// state exists before worker reclamation. Reordering this loses standalone
		// terminal wakeups on replace and can skip batched retries entirely.
		if age >= s.opts.ReplaceAfter && !lease.replaced {
			if !lease.timedOut {
				lease.timedOut = true
				timeouts = append(timeouts, leaseTimeoutEvent{
					batch:      lease.batch,
					microBatch: lease.microBatch,
					leaseID:    lease.leaseID,
					label:      lease.label,
					start:      lease.start,
					end:        lease.end,
				})
			}
			lease.replaced = true
			replaces = append(replaces, replaceEvent{
				batch:      lease.batch,
				microBatch: lease.microBatch,
				leaseID:    lease.leaseID,
				label:      lease.label,
				start:      lease.start,
				end:        lease.end,
			})
		}
	}
	s.timeoutBuf = timeouts[:0]
	s.mu.Unlock()

	return leaseEvents{
		timeouts: timeouts,
		warns:    warns,
		replaces: replaces,
	}
}

func (s *statScheduler) leaseLogTarget(batch *batchController, label string) any {
	if batch != nil && batch.standalone {
		return s.owner
	}
	return s.logTargetLabel(label)
}

func (s *statScheduler) processEvents(events leaseEvents) {
	for _, event := range events.timeouts {
		target := s.leaseLogTarget(event.batch, event.label)
		if event.batch.standaloneCanceled() {
			s.timeouts.Add(1)
			reclaimed := s.reclaimLeaseWorker(event.batch, event.microBatch, event.leaseID)
			if reclaimed && s.spawnWorker() {
				fs.Infof(target, "standalone stat %q canceled after lease timeout %v - reclaimed worker slot", event.label, s.opts.LeaseTimeout)
			} else if reclaimed {
				fs.Infof(target, "standalone stat %q canceled after lease timeout %v during shutdown", event.label, s.opts.LeaseTimeout)
			}
			continue
		}
		handled := event.batch.markLeaseTimedOut(event.microBatch, event.leaseID)
		if !handled {
			continue
		}
		s.timeouts.Add(1)
		if err := s.ctx.Err(); err != nil {
			fs.Infof(target, "stat microbatch [%d:%d) timed out after %v during shutdown - canceling batch", event.start, event.end, s.opts.LeaseTimeout)
			event.batch.cancelWithErr(err)
			continue
		}
		if event.batch.standalone {
			event.batch.notifyTimeout(&statLeaseTimeoutError{
				Path:    event.batch.standalonePath,
				Lstat:   event.batch.standaloneLstat,
				Timeout: s.opts.LeaseTimeout,
			})
		}
		fs.Infof(target, "stat microbatch [%d:%d) timed out after %v - retrying", event.start, event.end, s.opts.LeaseTimeout)
		s.scheduleRetry(event.batch, event.microBatch)
	}

	for _, event := range events.warns {
		fs.Infof(s.leaseLogTarget(event.batch, event.label), "stat microbatch [%d:%d) still running after %v", event.start, event.end, event.age.Round(time.Second))
	}
	for _, event := range events.replaces {
		target := s.leaseLogTarget(event.batch, event.label)
		reclaimed := s.reclaimLeaseWorker(event.batch, event.microBatch, event.leaseID)
		if reclaimed && s.spawnWorker() {
			fs.Errorf(target, "stat microbatch [%d:%d) appears stuck - reclaimed worker slot and spawned replacement worker", event.start, event.end)
			continue
		}
		if reclaimed {
			fs.Errorf(target, "stat microbatch [%d:%d) appears stuck - reclaimed worker slot during shutdown", event.start, event.end)
		}
	}
}

// reclaimLeaseWorker transfers wg.Done responsibility for one worker generation
// from finishWorker to this reclaim path. lease.workerReclaimed and the
// generation/accounted checks make that handoff one-way, so each spawn calls
// wg.Done exactly once here or in finishWorker, never both.
func (s *statScheduler) reclaimLeaseWorker(batch *batchController, microBatch int, leaseID uint64) bool {
	key := statLeaseKey{
		batch:      batch,
		microBatch: microBatch,
		leaseID:    leaseID,
	}

	s.mu.Lock()
	lease := s.active[key]
	if lease == nil || lease.workerReclaimed {
		s.mu.Unlock()
		return false
	}

	lease.workerReclaimed = true
	if lease.workerID < 0 || lease.workerID >= len(s.workers) {
		s.mu.Unlock()
		return false
	}

	worker := &s.workers[lease.workerID]
	if worker.generation != lease.workerGeneration || !worker.accounted {
		s.mu.Unlock()
		return false
	}

	s.workers[lease.workerID] = statWorkerState{}
	s.freeWorkerIDs = append(s.freeWorkerIDs, lease.workerID)
	s.workerCount--
	s.mu.Unlock()

	s.wg.Done()
	return true
}

func (s *statScheduler) watchdog() {
	ticker := time.NewTicker(s.watchdogTickInterval())
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}

		events := s.sweepLeases(time.Now())
		s.processEvents(events)
	}
}

// Retry-delay timers are part of scheduler lifetime and must be accounted in
// s.wg; otherwise Close can return while a delayed retry wakes up and enqueues
// more work after shutdown has started.
func (s *statScheduler) scheduleRetry(batch *batchController, microBatch int) {
	timer := newStatSchedulerRetryTimer(batch.retryBackoff(microBatch, s.opts.RetryBackoff))
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer stopTimer(timer)

		select {
		case <-timer.C:
		case <-s.ctx.Done():
			batch.cancelWithErr(s.ctx.Err())
			return
		}

		job := s.getJob()
		ok := batch.acquireRetryLease(microBatch, job)
		if !ok {
			s.putJob(job)
			return
		}
		if err := s.enqueueRetry(job.batch.ctx, job); err != nil {
			batch.revertIssuedLease(job.microBatch, job.leaseID, true)
			s.putJob(job)
			batch.cancelWithErr(err)
			return
		}
		s.retries.Add(1)
	}()
}
