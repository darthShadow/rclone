package local

import (
	"context"
	"os"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListCachedFileInfos_ShortFirstBatchSkipsPrefetchReader(t *testing.T) {
	f, root := newTestLocalFs(t)
	writeTestFile(t, root, "alpha.txt")
	writeTestFile(t, root, "beta.txt")

	originalNewPrefetchReader := newListPrefetchReader
	prefetchCalled := false
	newListPrefetchReader = func(ctx context.Context, owner *Fs, fd *os.File) *prefetchReader {
		prefetchCalled = true
		return originalNewPrefetchReader(ctx, owner, fd)
	}
	t.Cleanup(func() {
		newListPrefetchReader = originalNewPrefetchReader
	})

	fd, err := os.Open(root)
	require.NoError(t, err)

	var gotNames []string
	err = f.listCachedFileInfos(context.Background(), fd, nil, "", nil, func(entry *cachedDirEntry, nameBuf []byte) (os.FileInfo, []byte, error) {
		fi, err := entry.Info()
		return fi, nameBuf, err
	}, func(_ []cachedDirEntry, fis []statFileInfo) error {
		for i := range fis {
			if fis[i].fi == nil {
				continue
			}
			gotNames = append(gotNames, fis[i].Name())
		}
		return nil
	})
	require.NoError(t, err)

	slices.Sort(gotNames)
	assert.Equal(t, []string{"alpha.txt", "beta.txt"}, gotNames)
	assert.False(t, prefetchCalled)
}
