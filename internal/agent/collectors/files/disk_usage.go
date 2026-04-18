package files

import (
	"bufio"
	"context"
	"os"
	"strings"

	"github.com/vasyakrg/recon/internal/agent/collect"
)

func init() { collect.Register(&diskUsage{}) }

type diskUsage struct{}

type Mount struct {
	Source      string  `json:"source"`
	MountPoint  string  `json:"mount_point"`
	FSType      string  `json:"fstype"`
	TotalB      uint64  `json:"total_bytes"`
	FreeB       uint64  `json:"free_bytes"`
	AvailB      uint64  `json:"avail_bytes"`
	UsedPct     float64 `json:"used_pct"`
	InodesTotal uint64  `json:"inodes_total"`
	InodesFree  uint64  `json:"inodes_free"`
}

type DiskResult struct {
	Mounts []Mount `json:"mounts"`
}

// fsTypesOfInterest filters /proc/mounts to real local filesystems. Network
// and pseudo filesystems are skipped to keep output focused.
var fsTypesOfInterest = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true,
	"xfs": true, "btrfs": true, "zfs": true,
	"f2fs": true, "reiserfs": true, "jfs": true,
	"vfat": true, "exfat": true, "ntfs": true,
}

func (diskUsage) Manifest() collect.Manifest {
	return collect.Manifest{
		Name:        "disk_usage",
		Version:     "1.0.0",
		Category:    "system",
		Description: "Per-mount disk usage (bytes + inodes) for local filesystems. Reads /proc/mounts and statfs(2). Hints when free space < 10%.",
		Reads:       []string{"/proc/mounts", "statfs(2)"},
	}
}

func (diskUsage) Run(_ context.Context, _ collect.Params) (collect.Result, error) {
	out := DiskResult{}
	hints := []collect.Hint{}

	mounts, err := parseMounts("/proc/mounts")
	if err != nil {
		return collect.Result{}, err
	}

	for _, m := range mounts {
		if !fsTypesOfInterest[m.FSType] {
			continue
		}
		filled, err := statMount(m)
		if err != nil {
			continue
		}
		out.Mounts = append(out.Mounts, filled)
		if filled.UsedPct >= 90 {
			hints = append(hints, collect.Hint{
				Severity: "warn", Code: "disk.almost_full",
				Message:  filled.MountPoint + " is " + formatPct(filled.UsedPct),
				Evidence: map[string]any{"mount": filled.MountPoint, "used_pct": filled.UsedPct},
			})
		}
	}

	return collect.Result{Data: out, Hints: hints}, nil
}

// parseMounts reads /proc/mounts (or compatible) into Mount structs without
// statfs filling.
func parseMounts(path string) ([]Mount, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []Mount
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		out = append(out, Mount{Source: fields[0], MountPoint: fields[1], FSType: fields[2]})
	}
	return out, sc.Err()
}

func formatPct(p float64) string {
	return strconvF(p, 1) + "% used"
}

// strconvF formats a float with n decimal places without pulling in
// strconv (avoids adding to the import set used elsewhere).
func strconvF(v float64, n int) string {
	// Trivial implementation good enough for "X.Y%" style output.
	mult := 1.0
	for i := 0; i < n; i++ {
		mult *= 10
	}
	rounded := int64(v*mult + 0.5)
	whole := rounded / int64(mult)
	frac := rounded % int64(mult)
	if n == 0 {
		return itoa(whole)
	}
	fracStr := itoa(frac)
	for len(fracStr) < n {
		fracStr = "0" + fracStr
	}
	return itoa(whole) + "." + fracStr
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	out := []byte{}
	for v > 0 {
		out = append([]byte{byte('0' + v%10)}, out...)
		v /= 10
	}
	if neg {
		out = append([]byte{'-'}, out...)
	}
	return string(out)
}
