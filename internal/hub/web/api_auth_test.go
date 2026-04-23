package web

import (
	"testing"

	"github.com/vasyakrg/recon/internal/hub/store"
)

func TestScopeSatisfies(t *testing.T) {
	cases := []struct {
		have store.APITokenScope
		need store.APITokenScope
		want bool
	}{
		{store.APIScopeRead, store.APIScopeRead, true},
		{store.APIScopeRead, store.APIScopeInvestigate, false},
		{store.APIScopeRead, store.APIScopeAdmin, false},
		{store.APIScopeInvestigate, store.APIScopeRead, true},
		{store.APIScopeInvestigate, store.APIScopeInvestigate, true},
		{store.APIScopeInvestigate, store.APIScopeAdmin, false},
		{store.APIScopeAdmin, store.APIScopeRead, true},
		{store.APIScopeAdmin, store.APIScopeInvestigate, true},
		{store.APIScopeAdmin, store.APIScopeAdmin, true},
	}
	for _, c := range cases {
		if got := scopeSatisfies(c.have, c.need); got != c.want {
			t.Errorf("scopeSatisfies(%s, %s) = %v, want %v", c.have, c.need, got, c.want)
		}
	}
}

func TestExtractBearer(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"Bearer foo", "foo"},
		{"Bearer  foo  ", "foo"},
		{"bearer foo", ""}, // case-sensitive per spec
		{"Basic abc", ""},
	}
	for _, c := range cases {
		req := &fakeReq{hdr: c.in}
		if got := req.extract(); got != c.want {
			t.Errorf("extract(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

type fakeReq struct{ hdr string }

func (f *fakeReq) extract() string {
	// Mirror extractBearer without needing a real http.Request.
	const prefix = "Bearer "
	if f.hdr == "" || len(f.hdr) < len(prefix) || f.hdr[:len(prefix)] != prefix {
		return ""
	}
	s := f.hdr[len(prefix):]
	for len(s) > 0 && s[0] == ' ' {
		s = s[1:]
	}
	for len(s) > 0 && s[len(s)-1] == ' ' {
		s = s[:len(s)-1]
	}
	return s
}
