package server

import (
	"fmt"
	"net/http/httptest"
	"testing"

	"brood.dev/go/brood/proxmox"
)

// FEATURE: Public managed-provider failures carry a safe actionable reason.
// SPEC: Typed ownership/uncertainty/task refusals are conflicts, permission
// failures are unavailable, and provider credentials never enter diagnostics.
func TestManagedProviderErrorsAreActionable(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{{proxmox.ErrOwnership, 409}, {proxmox.ErrUncertain, 409}, {proxmox.ErrTaskFailed, 409}, {proxmox.ErrPermission, 503}} {
		w := httptest.NewRecorder()
		managedAPIError(w, httptest.NewRequest("POST", "/", nil), fmt.Errorf("wrapped: %w", tc.err))
		if w.Code != tc.status {
			t.Fatalf("%v status %d, want %d: %s", tc.err, w.Code, tc.status, w.Body.String())
		}
	}
}
