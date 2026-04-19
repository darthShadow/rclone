package local

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/rclone/rclone/fs"
)

// prefetchReader overlaps ReadDir I/O for batch N+1 with foreground stat work
// on batch N. It takes sole ownership of the read fd until Close returns.
type prefetchReader struct {
	ch     chan readResult
	cancel context.CancelFunc

	closeFD sync.Once
	fd      *os.File
	mu      sync.Mutex
	closed  bool
}

var newListPrefetchReader = newPrefetchReader

// newPrefetchReader starts the background ReadDir loop and transfers sole
// ownership of fd to the returned reader.
func newPrefetchReader(ctx context.Context, owner *Fs, fd *os.File) *prefetchReader {
	runCtx, cancel := context.WithCancel(ctx)
	pr := &prefetchReader{
		ch:     make(chan readResult, 1),
		cancel: cancel,
		fd:     fd,
	}
	go pr.run(runCtx, owner, fd)
	return pr
}

// run reads directory batches, opens an optional batch-scoped stat fd, and
// hands completed readResult values to the foreground through a 1-deep buffer.
func (pr *prefetchReader) run(ctx context.Context, owner *Fs, fd *os.File) {
	pending := newReadResult()
	fdOwned := true

	defer close(pr.ch)
	defer pr.closeFD.Do(func() {
		if fdOwned && pr.fd != nil {
			_ = pr.fd.Close()
		}
	})
	defer func() {
		if r := recover(); r != nil {
			closeReadResultStatDir(&pending)

			var panicErr error
			if e, ok := r.(error); ok {
				panicErr = fmt.Errorf("panic reading directory: %w", e)
			} else {
				panicErr = fmt.Errorf("panic reading directory: %v", r)
			}

			fs.Errorf(owner, "prefetchReader: %v", panicErr)
			pr.trySend(ctx, readResult{err: panicErr, statDir: invalidStatDirFD})
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		batch := newReadResult()
		batch.entries, batch.err = fd.ReadDir(listReadDirBatchSize)
		pending = batch

		if len(batch.entries) > 0 {
			statDir, err := openDirAtReadFD(fd)
			if err != nil {
				fs.Debugf(owner, "openDirAtReadFD: %v, falling back to entry.Info()", err)
			} else {
				batch.statDir = statDir
				// pending holds the next read's scratch buffer; once batch has a
				// statDir fd, pending must carry the same fd so the follow-on read
				// inherits it.
				pending.statDir = statDir
			}
		}

		if pr.trySend(ctx, batch) {
			pending = newReadResult()
		} else {
			closeReadResultStatDir(&batch)
			return
		}

		if batch.err != nil {
			return
		}
	}
}

// Next blocks until the next prefetched batch is available or the caller
// context is canceled.
func (pr *prefetchReader) Next(ctx context.Context) (readResult, bool) {
	select {
	case batch, ok := <-pr.ch:
		return batch, ok
	case <-ctx.Done():
		return newReadResult(), false
	}
}

// Close synchronously closes fd before marking the reader closed, with the same
// mutex guarding both. That prevents late sends after Close returns and keeps
// leaked post-close batches from retaining statDir fds when ReadDir unwinds.
func (pr *prefetchReader) Close() {
	pr.cancel()
	pr.closeFD.Do(func() {
		if pr.fd != nil {
			_ = pr.fd.Close()
		}
	})
	pr.mu.Lock()
	pr.closed = true
	pr.drainReadyLocked()
	pr.mu.Unlock()
}

func (pr *prefetchReader) trySend(ctx context.Context, batch readResult) bool {
	pr.mu.Lock()
	defer pr.mu.Unlock()

	if pr.closed {
		return false
	}

	select {
	case pr.ch <- batch:
		return true
	case <-ctx.Done():
		return false
	}
}

func (pr *prefetchReader) drainReadyLocked() {
	for {
		select {
		case batch, ok := <-pr.ch:
			if !ok {
				return
			}
			closeReadResultStatDir(&batch)
		default:
			return
		}
	}
}
