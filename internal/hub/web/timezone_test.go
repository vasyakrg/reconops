package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vasyakrg/recon/internal/hub/store"
)

func TestFormatUserTZ(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation (tzdata embedded?): %v", err)
	}
	// Noon UTC in January → EST (UTC-5), no DST ambiguity.
	ts := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		loc    *time.Location
		t      time.Time
		layout string
		want   string
	}{
		{time.UTC, ts, "2006-01-02 15:04 MST", "2026-01-15 12:00 UTC"},
		{ny, ts, "2006-01-02 15:04 MST", "2026-01-15 07:00 EST"},
		{ny, ts, "15:04", "07:00"},
		{nil, ts, "15:04 MST", "12:00 UTC"},   // nil loc → UTC
		{time.UTC, time.Time{}, "15:04", "—"}, // zero time → em dash
	}
	for _, c := range cases {
		if got := formatUserTZ(c.loc, c.t, c.layout); got != c.want {
			t.Errorf("formatUserTZ(%v,%v,%q) = %q, want %q", c.loc, c.t, c.layout, got, c.want)
		}
	}
}

func TestValidTZName(t *testing.T) {
	for _, ok := range []string{"UTC", "America/New_York", "Asia/Kolkata", "Etc/GMT+5", "Europe/Paris"} {
		if !validTZName(ok) {
			t.Errorf("validTZName(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "../etc/passwd", "a b", "zone;rm", strings.Repeat("x", 65), "zone\n"} {
		if validTZName(bad) {
			t.Errorf("validTZName(%q) = true, want false", bad)
		}
	}
}

func TestUserLocation(t *testing.T) {
	mk := func(tz string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if tz != "" {
			r.AddCookie(&http.Cookie{Name: cookieTZ, Value: tz})
		}
		return r
	}
	if loc := userLocation(mk("")); loc != time.UTC {
		t.Errorf("missing cookie → want UTC, got %v", loc)
	}
	if loc := userLocation(mk("Asia/Kolkata")); loc.String() != "Asia/Kolkata" {
		t.Errorf("valid cookie → want Asia/Kolkata, got %v", loc)
	}
	if loc := userLocation(mk("Mars/Phobos")); loc != time.UTC {
		t.Errorf("unknown-but-clean zone → want UTC fallback, got %v", loc)
	}
	if loc := userLocation(mk("../../etc")); loc != time.UTC {
		t.Errorf("malformed zone → want UTC fallback, got %v", loc)
	}
}

// Regression: the browser sets the cookie via encodeURIComponent, so the
// server must percent-decode it. Without decoding, every multi-component zone
// falls back to UTC and T6 is silently dead. These feed the EXACT bytes a
// browser sends (r.Header "Cookie"), not an un-encoded AddCookie value.
func TestUserLocation_PercentEncodedCookie(t *testing.T) {
	mk := func(rawCookie string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Cookie", rawCookie)
		return r
	}
	if loc := userLocation(mk("tz=America%2FNew_York")); loc.String() != "America/New_York" {
		t.Errorf("encodeURIComponent(America/New_York) → want America/New_York, got %v", loc)
	}
	// '+' must survive (PathUnescape, not QueryUnescape): Etc/GMT+5 → encodeURIComponent → Etc%2FGMT%2B5.
	if loc := userLocation(mk("tz=Etc%2FGMT%2B5")); loc.String() != "Etc/GMT+5" {
		t.Errorf("encodeURIComponent(Etc/GMT+5) → want Etc/GMT+5, got %v", loc)
	}
	// A garbage percent-encoding that decodes to a bad name still falls back to UTC.
	if loc := userLocation(mk("tz=%2E%2E%2Fetc")); loc != time.UTC {
		t.Errorf("encoded traversal → want UTC fallback, got %v", loc)
	}
}

// End-to-end: the same investigation, rendered with different tz cookies, must
// carry differently-localized timestamps — proving cookie → UserTZ → template.
func TestDetailPageLocalizesTimestamps(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-tz", Goal: "g", Status: "active", CreatedBy: "op", Model: "m", BaseURL: "u",
	}); err != nil {
		t.Fatal(err)
	}
	inv, err := st.GetInvestigation(ctx, "inv-tz")
	if err != nil {
		t.Fatal(err)
	}
	ny, _ := time.LoadLocation("America/New_York")
	wantUTC := formatUserTZ(time.UTC, inv.CreatedAt, "2006-01-02 15:04:05 MST")
	wantNY := formatUserTZ(ny, inv.CreatedAt, "2006-01-02 15:04:05 MST")
	if wantUTC == wantNY {
		t.Fatalf("test setup: UTC and NY renders coincide (%q) — pick a non-UTC offset", wantUTC)
	}
	sid, _, err := srv.sessions.issue(ctx, "admin", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	get := func(tz string) string {
		req := httptest.NewRequest(http.MethodGet, "/investigations/inv-tz", nil)
		req.AddCookie(&http.Cookie{Name: cookieSession, Value: sid})
		if tz != "" {
			req.AddCookie(&http.Cookie{Name: cookieTZ, Value: tz})
		}
		rw := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rw, req)
		if rw.Code != http.StatusOK {
			t.Fatalf("want 200 (tz=%q), got %d", tz, rw.Code)
		}
		return rw.Body.String()
	}

	if body := get(""); !strings.Contains(body, wantUTC) {
		t.Fatalf("no-cookie render missing UTC timestamp %q", wantUTC)
	}
	if body := get("America/New_York"); !strings.Contains(body, wantNY) {
		t.Fatalf("NY-cookie render missing localized timestamp %q", wantNY)
	}
}
