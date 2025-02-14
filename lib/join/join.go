// Package join provides local copies of the path.Join() and filepath.Join() functions
// with a pool-backed Buffer to reduce allocation.
package join

// PathJoin - like path.Join() but with a pool-backed Buffer to reduce allocation.
func PathJoin(elements ...string) string {
	return unixBufferPathJoin(elements...)
}

// FilePathJoin - like filepath.Join() but with a pool-backed Buffer to reduce allocation.
func FilePathJoin(elements ...string) string {
	return filePathJoin(elements...)
}
