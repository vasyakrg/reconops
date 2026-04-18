//go:build !linux

package files

// statMount is a stub on non-Linux: returns the mount with zero usage so the
// dev build on macOS compiles. Production agents only run on Linux.
func statMount(m Mount) (Mount, error) {
	return m, nil
}
