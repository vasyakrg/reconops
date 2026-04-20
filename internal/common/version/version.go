package version

import "strings"

var (
	Version = "0.1.0-dev"
	Commit  = "dev"
)

func Full() string {
	return Version + "+" + Commit
}

// Outdated reports whether `current` is strictly below `latest` using a
// loose semver comparison (vMAJOR.MINOR.PATCH; suffixes ignored). Empty
// or all-zero builds return false so a developer running a brand-new
// repo with no tags is never flagged as outdated. Dev builds with a
// real-looking version string (e.g. "0.1.7-dirty") DO get compared —
// the version stamp from `git describe` is the source of truth.
func Outdated(current, latest string) bool {
	if current == "" || latest == "" {
		return false
	}
	if strings.HasPrefix(current, "0.0.0") {
		return false
	}
	return compareSemver(current, latest) < 0
}

func compareSemver(a, b string) int {
	aa := splitSemver(a)
	bb := splitSemver(b)
	for i := 0; i < 3; i++ {
		var ai, bi int
		if i < len(aa) {
			ai = aa[i]
		}
		if i < len(bb) {
			bi = bb[i]
		}
		switch {
		case ai < bi:
			return -1
		case ai > bi:
			return 1
		}
	}
	return 0
}

func splitSemver(s string) []int {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n := 0
		for _, c := range p {
			if c < '0' || c > '9' {
				break
			}
			n = n*10 + int(c-'0')
		}
		out = append(out, n)
	}
	return out
}
