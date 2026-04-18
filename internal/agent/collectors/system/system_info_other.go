//go:build !linux

package system

import "runtime"

func unameRelease() (string, error) {
	return runtime.GOOS + "/dev", nil
}
