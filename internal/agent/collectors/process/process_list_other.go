//go:build !linux

package process

// readProcesses returns an empty list on non-Linux. Production agents only
// run on Linux; the dev build needs this so the package compiles.
func readProcesses(_ string) ([]Process, error) {
	return nil, nil
}
