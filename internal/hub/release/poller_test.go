package release

import "testing"

func TestParseRepo(t *testing.T) {
	cases := []struct {
		in    string
		owner string
		repo  string
		ok    bool
	}{
		{"https://github.com/vasyakrg/reconops", "vasyakrg", "reconops", true},
		{"https://github.com/vasyakrg/reconops/", "vasyakrg", "reconops", true},
		{"https://github.com/vasyakrg/reconops.git", "vasyakrg", "reconops", true},
		{"https://github.com/vasyakrg/reconops/releases", "vasyakrg", "reconops", true},
		{"https://gitlab.com/x/y", "", "", false},
		{"", "", "", false},
		{"https://github.com/only", "", "", false},
	}
	for _, c := range cases {
		o, r, ok := parseRepo(c.in)
		if ok != c.ok || o != c.owner || r != c.repo {
			t.Fatalf("parseRepo(%q)=(%q,%q,%v), want (%q,%q,%v)", c.in, o, r, ok, c.owner, c.repo, c.ok)
		}
	}
}

func TestOutdated(t *testing.T) {
	cases := []struct {
		cur, lat string
		want     bool
	}{
		{"v0.1.3", "v0.1.4", true},
		{"v0.1.4", "v0.1.4", false},
		{"v0.2.0", "v0.1.4", false},
		{"v0.1.4-rc1", "v0.1.4", false},
		{"", "v0.1.4", false},
		{"v0.1.4", "", false},
		{"0.1.0-dev+abc", "v0.1.4", false}, // dev build → not flagged
		{"v1.0.0", "v2.0.0", true},
	}
	for _, c := range cases {
		if got := Outdated(c.cur, c.lat); got != c.want {
			t.Fatalf("Outdated(%q,%q)=%v want %v", c.cur, c.lat, got, c.want)
		}
	}
}
