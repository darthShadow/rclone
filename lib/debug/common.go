// Package debug contains functions for dealing with runtime/debug functions across go versions
package debug

import (
	"runtime/debug"
)

// SetGCPercent calls the runtime/debug.SetGCPercent function to set the garbage
// collection percentage.
func SetGCPercent(percent int) int {
	return debug.SetGCPercent(percent)
}

// SetMemoryLimit calls the runtime/debug.SetMemoryLimit function to set the
// soft-memory limit.
func SetMemoryLimit(limit int64) int64 {
	return debug.SetMemoryLimit(limit)
}

// FreeOSMemory calls the runtime/debug.FreeOSMemory function to free memory
// that is no longer in use.
func FreeOSMemory() {
	debug.FreeOSMemory()
}
