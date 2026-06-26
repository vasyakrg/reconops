package web

import (
	"strings"
	"testing"
)

func TestInvestigatorAvailabilityUsesReasonSpecificHints(t *testing.T) {
	t.Parallel()

	availability := NewInvestigatorAvailability(nil,
		`llm: base_url "http://192.168.202.10:4000/v1" is plaintext HTTP and not loopback; set RECON_LLM_ALLOW_INSECURE_HTTP=true only for private/link-local router endpoints`)

	if availability.Enabled {
		t.Fatal("disabled LLM availability should not be enabled")
	}
	if availability.DisabledReason != "insecure_base_url" {
		t.Fatalf("want insecure_base_url, got %q", availability.DisabledReason)
	}
	for _, want := range []string{
		"RECON_LLM_BASE_URL is plaintext HTTP",
		"RECON_LLM_ALLOW_INSECURE_HTTP=true",
		"private/link-local router IP",
	} {
		if !strings.Contains(availability.ConfigHint, want) {
			t.Fatalf("hint missing %q: %s", want, availability.ConfigHint)
		}
	}
	if strings.Contains(availability.ConfigHint, "Set RECON_LLM_API_KEY, RECON_LLM_BASE_URL, and RECON_LLM_MODEL as needed") {
		t.Fatalf("insecure_base_url should not use the generic recovery hint: %s", availability.ConfigHint)
	}
}
