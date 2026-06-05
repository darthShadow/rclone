//go:build linux

package local

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// TestObjectFdDupIndependence verifies that Object.Fd returns a dup fd that
// remains valid after the internally opened *os.File has been closed.
// This confirms F_DUPFD_CLOEXEC produces an independent file descriptor.
func TestObjectFdDupIndependence(t *testing.T) {
	content := []byte("fd-dup-test-content")

	tmp, err := os.CreateTemp(t.TempDir(), "fd_linux_test")
	require.NoError(t, err)
	_, err = tmp.Write(content)
	require.NoError(t, err)
	require.NoError(t, tmp.Close())

	// Build a minimal Object pointing at the temp file.
	o := &Object{path: tmp.Name()}

	// Call Fd; the implementation opens the file, dups it, then closes the
	// internal *os.File before returning.
	fdVal, err := o.Fd(context.Background(), os.O_RDONLY)
	require.NoError(t, err)
	require.NotEqual(t, uintptr(0), fdVal, "Fd must not return 0 on success")

	// The dup fd must be >= 3 (F_DUPFD_CLOEXEC with min=3).
	assert.GreaterOrEqual(t, int(fdVal), 3, "dup fd should be >= 3")

	// Verify the dup fd is still valid: read from it and confirm content.
	fd := int(fdVal)
	buf := make([]byte, len(content))
	n, readErr := unix.Read(fd, buf)
	require.NoError(t, readErr)
	assert.Equal(t, content, buf[:n])

	// Caller is responsible for closing. Close and verify the fd is now gone.
	require.NoError(t, unix.Close(fd))
	_, fcntlErr := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	assert.Error(t, fcntlErr, "fd should be invalid after Close")
}
