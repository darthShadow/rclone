package join

import (
	"path"

	bufferPool "github.com/libp2p/go-buffer-pool"
)

// unixBufferPathJoin implements the logic from path.Join() but with a pool-backed Buffer to reduce allocation.
// References:
// - https://github.com/minio/minio/pull/17036
// - https://github.com/minio/minio/pull/17960
// - https://pkg.go.dev/path#Join
// - https://cs.opensource.google/go/go/+/refs/tags/go1.22rc2:src/path/path.go;l=155
func unixBufferPathJoin(elements ...string) string {
	size := 0

	for _, element := range elements {
		size += len(element)
	}

	if size == 0 {
		return ""
	}

	size += len(unixSlashSeparator) * (len(elements) - 1)

	dst := bufferPool.NewBuffer(nil)
	dst.Grow(size)
	defer dst.Reset()

	added := 0

	//nolint:errcheck
	//goland:noinspection GoUnhandledErrorResult
	for _, e := range elements {
		if added > 0 || e != "" {
			if added > 0 {
				dst.WriteString(unixSlashSeparator)
			}
			dst.WriteString(e)
			added += len(e)
		}
	}

	return path.Clean(dst.String())
}
