package server

// BUG: Sandboxing the trusted mode-selection page gives it an opaque origin,
// which can strip SameSite session cookies from iframe navigation to ticket issuance.
// HYPOTHESIS: Guest-content sandbox policy was also applied to trusted navigation.
// SOLUTION: Keep the trusted selector same-origin under a no-script content policy;
// guest actor-view and readonly response sandbox policies remain unchanged.
// RESULT: FIXED — HTTP policy regression passes; real-browser acceptance is separate.
// STATUS: FIXED

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTerminalModeChoiceRetainsAuthenticatedNavigationPolicy(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/swarms/sw/proxy/actors/worker/?mode=", nil)
	w := httptest.NewRecorder()
	serveTerminalModeChoice(w, r)
	policy := w.Header().Get("Content-Security-Policy")
	if strings.Contains(policy, "sandbox") {
		t.Fatal("trusted mode navigation must not acquire an opaque sandbox origin; guest content alone is sandboxed")
	}
	if !strings.Contains(policy, "default-src 'none'") || !strings.Contains(policy, "frame-ancestors 'self'") {
		t.Fatalf("trusted choice page lost content restrictions: %s", policy)
	}
	if !strings.Contains(w.Body.String(), "?mode=rw") || !strings.Contains(w.Body.String(), "?mode=ro") {
		t.Fatal("mode choices are not authenticated relative navigations")
	}
}
