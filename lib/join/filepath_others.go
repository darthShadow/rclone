//go:build !unix && !(js && wasm) && !wasip1

package join

import (
	"path/filepath"
)

func filePathJoin(elements ...string) string {
	return filepath.Join(elements...)
}
