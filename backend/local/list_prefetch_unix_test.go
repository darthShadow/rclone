//go:build !windows && !plan9 && !js

package local

import (
	"errors"
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestPrefetchReader_CloseClosesFDImmediately(t *testing.T) {
	dir := t.TempDir()
	fd, err := os.Open(dir)
	require.NoError(t, err)

	savedFD := int(fd.Fd())
	pr := &prefetchReader{
		cancel: func() {},
		fd:     fd,
		ch:     make(chan readResult),
	}

	pr.Close()
	pr.Close()

	err = unix.Fstat(savedFD, &unix.Stat_t{})
	require.True(t, errors.Is(err, syscall.EBADF), "expected prefetch reader Close to synchronously close the read fd, got %v", err)
}
