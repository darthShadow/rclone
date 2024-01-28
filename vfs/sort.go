//go:build !go1.21

package vfs

import (
	"sort"
)

func sortNodes(items Nodes) {
	sort.Sort(items)
}
