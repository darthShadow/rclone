//go:build unix || (js && wasm) || wasip1

package join

import (
	"path/filepath"

	bufferPool "github.com/libp2p/go-buffer-pool"
)

// unixBufferFilePathJoin implements the logic from filepath.Join() but with a pool-backed Buffer to reduce allocation.
// References:
// - https://github.com/minio/minio/pull/17036
// - https://github.com/minio/minio/pull/17960
// - https://pkg.go.dev/path/filepath#Join
// - https://pkg.go.dev/strings#Join
// - https://cs.opensource.google/go/go/+/refs/tags/go1.22rc2:src/strings/strings.go;l=429
func unixBufferFilePathJoin(elements ...string) string {
	switch len(elements) {
	case 0:
		return ""
	case 1:
		return elements[0]
	}

	size := 0

	if len(unixSlashSeparator) >= maxInt/(len(elements)-1) {
		panic("strings: Join output length overflow")
	}
	size += len(unixSlashSeparator) * (len(elements) - 1)

	for _, element := range elements {
		if len(element) > maxInt-size {
			panic("join: output length overflow")
		}
		size += len(element)
	}

	dst := bufferPool.NewBuffer(nil)
	dst.Grow(size)
	defer dst.Reset()

	//nolint:errcheck
	//goland:noinspection GoUnhandledErrorResult
	dst.WriteString(elements[0])

	//nolint:errcheck
	//goland:noinspection GoUnhandledErrorResult
	for _, e := range elements[1:] {
		dst.WriteString(unixSlashSeparator)
		dst.WriteString(e)
	}

	return dst.String()
}

func filePathJoin(elements ...string) string {
	for i, e := range elements {
		if e != "" {
			return filepath.Clean(unixBufferFilePathJoin(elements[i:]...))
		}
	}

	return ""
}
