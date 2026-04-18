//go:build linux

package files

import "syscall"

func statMount(m Mount) (Mount, error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(m.MountPoint, &s); err != nil {
		return m, err
	}
	bsize := uint64(s.Bsize) //nolint:gosec // Bsize is non-negative on Linux
	m.TotalB = s.Blocks * bsize
	m.FreeB = s.Bfree * bsize
	m.AvailB = s.Bavail * bsize
	if m.TotalB > 0 {
		used := m.TotalB - m.FreeB
		m.UsedPct = float64(used) * 100 / float64(m.TotalB)
	}
	m.InodesTotal = s.Files
	m.InodesFree = s.Ffree
	return m, nil
}
