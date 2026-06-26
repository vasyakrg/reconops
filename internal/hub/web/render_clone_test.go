package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vasyakrg/recon/internal/hub/store"
)

// TestFragmentRenderDoesNotPoisonClone guards the html/template Clone
// discipline: live-update fragments must render on a clone, never on the
// shared root. Before the fix the fragments handler executed s.tpl directly,
// after which every subsequent full-page render failed with
// "html/template: cannot Clone \"\" after it has executed" (e.g. the redirect
// right after clicking Approve).
func TestFragmentRenderDoesNotPoisonClone(t *testing.T) {
	srv, st := newTestServer(t)
	srv.log = webTestLogger()
	id := "inv_frag01"
	if err := st.InsertInvestigation(context.Background(), store.Investigation{
		ID: id, Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.tpl.Clone(); err != nil {
		t.Fatalf("precondition: base template not clonable: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/investigations/fragments/"+id, nil)
	rw := httptest.NewRecorder()
	srv.handleInvestigationFragments(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("fragment render code=%d body=%s", rw.Code, rw.Body.String())
	}

	if _, err := srv.tpl.Clone(); err != nil {
		t.Fatalf("base template Clone() poisoned after fragment render: %v", err)
	}
}

// T13 embed check: the SERVED hub.js must contain the T6 timezone-detection
// code. An embed.FS asset only changes after an image REBUILD (not a restart);
// this catches a deploy that shipped stale JS, and pins that the new bundle
// actually sets the tz cookie. assetVersion() also rolls when hub.js changes, so
// the ?v= buster forces browsers off the cached old file.
func TestServedHubJSEmbedsTZDetection(t *testing.T) {
	srv, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/static/hub.js", nil)
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("serve /static/hub.js: %d", rw.Code)
	}
	body := rw.Body.String()
	for _, want := range []string{"resolvedOptions().timeZone", "encodeURIComponent(tz)", "location.reload()"} {
		if !strings.Contains(body, want) {
			t.Fatalf("served hub.js missing TZ-detection %q — embed not rebuilt with the new JS?", want)
		}
	}
}

// TestAssetURLsAreCacheBusted guards that hub.js/hub.css are loaded with a
// content-versioned query param. Without it, a deploy that changes hub.js
// leaves the browser serving the cached old file (Cache-Control max-age),
// which is exactly how a fixed UI keeps looking broken after a deploy.
func TestAssetURLsAreCacheBusted(t *testing.T) {
	if assetVersion() == "" {
		t.Fatal("assetVersion() must be non-empty")
	}
	for _, name := range []string{"layout.html", "login.html"} {
		b, err := os.ReadFile(filepath.Join("templates", name))
		if err != nil {
			t.Fatal(err)
		}
		body := string(b)
		// Every /static/hub.* reference must carry the ?v={{assetVer}} buster.
		for _, ref := range []string{`/static/hub.js"`, `/static/hub.css"`} {
			if strings.Contains(body, ref) {
				t.Fatalf("%s references %s without a ?v={{assetVer}} cache-buster", name, ref)
			}
		}
	}
}
