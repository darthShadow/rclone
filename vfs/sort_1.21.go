//go:build go1.21

package vfs

import (
	"bytes"
	"slices"
)

func sortNodes(items Nodes) {
	/**
	  References:
	    - https://aead.dev/news/sort-strings/
	    - https://github.com/golang/go/issues/61725
	    - https://twitter.com/go100and1/status/1583715982904029185
	    - https://www.ompluscator.com/article/golang/standard-lib-slices-part2/
	    - https://go101.org/blog/2022-10-01-three-way-string-comparison.html
	    - https://github.com/golang/go/issues/50167
	    - https://github.com/golang/go/blob/d28bf6c9a2ea9b992796738d03eb3d15ffbfc0b4/src/internal/bytealg/compare_generic.go#L39
	    - https://news.ycombinator.com/item?id=33316402
	    - https://github.com/grafana/mimir/pull/6312/commits/d45afe9c569e80edc6758d69013a723a05326713
	    - https://github.com/google/gvisor/pull/9693/files#r1398508407
	*/
	slices.SortFunc(items, func(a, b Node) int {
		return bytes.Compare(a.NameBytes(), b.NameBytes())
	})
}
