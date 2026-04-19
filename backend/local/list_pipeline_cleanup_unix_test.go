//go:build !windows

package local

import (
	"context"
	"errors"
	"os"
	"runtime"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestListController_ProcessBatch_ClosesStatDirOnPanic(t *testing.T) {
	dir := t.TempDir()
	statDir, err := os.Open(dir)
	require.NoError(t, err)
	defer func() {
		_ = statDir.Close()
	}()
	savedFD := int(statDir.Fd())

	batch := newReadResult()
	batch.entries = fakeEntries("panic.txt")
	batch.statDir = savedFD
	controller := newListController(nil, nil, listControllerOptions{})

	var recovered any
	func() {
		defer func() {
			recovered = recover()
		}()
		_ = controller.ProcessBatch(context.Background(), &batch, func(os.DirEntry) (cachedDirEntry, bool) {
			panic("boom")
		}, func(entry *cachedDirEntry, nameBuf []byte) (os.FileInfo, []byte, error) {
			t.Fatalf("unexpected stat call for %q", entry.Name())
			return nil, nameBuf, nil
		}, func([]cachedDirEntry, []statFileInfo) error {
			return nil
		})
	}()

	require.NotNil(t, recovered)
	require.Equal(t, invalidStatDirFD, batch.statDir)
	err = unix.Fstat(savedFD, &unix.Stat_t{})
	runtime.KeepAlive(statDir)
	require.True(t, errors.Is(err, syscall.EBADF), "expected raw stat dir fd to be closed after panic cleanup, got %v", err)
}
