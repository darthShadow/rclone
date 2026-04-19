package local

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestListFileInfos_CancellationLargeDirectory(t *testing.T) {
	f, root := newTestLocalFs(t)
	for i := 0; i < 2050; i++ {
		writeTestFile(t, root, fmt.Sprintf("item-%04d.txt", i))
	}

	ctx, cancel := context.WithCancel(context.Background())
	f.statScheduler.Close()
	f.statScheduler = newStatScheduler(f, statSchedulerOptions{
		Workers:          1,
		MaxWorkers:       1,
		QueueDepth:       16,
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
		_, err := f.listFileInfos(ctx, fd, nil, nil, func(entry os.DirEntry) os.FileInfo {
			select {
			case started <- struct{}{}:
			default:
			}
			<-ctx.Done()
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
		require.True(t, errors.Is(err, context.Canceled), "expected context cancellation, got %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for large-directory cancellation")
	}
}
