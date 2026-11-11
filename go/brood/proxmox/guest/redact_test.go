//go:build linux

package guest

import (
	"fmt"
	"strings"
	"testing"
)

// FEATURE: The guest attestor token stays out of every fmt verb (#231).
// SPEC: WorkspaceAttestor prints only its redacted form for any verb or flag.

// redactVerbs covers every fmt verb and the flag combinations a caller can
// write. %T and %p are handled by fmt before Format and are not listed; %w
// outside Errorf is a bad verb that fmt prints without Format.
func redactVerbs() []string {
	verbs := "vsqxXdtcbeEfFgGoOU"
	var out []string
	for _, v := range verbs {
		for _, flags := range []string{"", "+", "#", "-", " ", "0", "10", "-10", "+#"} {
			out = append(out, "%"+flags+string(v))
		}
	}
	return out
}

// requireRedacted formats each value with every verb and demands the exact
// redacted form, so a sentinel can appear neither alone nor nested.
func requireRedacted(t *testing.T, name, want string, sentinels []string, values ...any) {
	t.Helper()
	for _, v := range values {
		for _, verb := range redactVerbs() {
			got := fmt.Sprintf(verb, v)
			for _, s := range sentinels {
				if strings.Contains(got, s) {
					t.Fatalf("%s %s printed a secret: %q", name, verb, got)
				}
			}
			if !strings.Contains(got, want) {
				t.Fatalf("%s %s = %q, want redacted form %q", name, verb, got, want)
			}
		}
	}
}

func TestWorkspaceAttestorRedactsEveryFormatVerb(t *testing.T) {
	const tok = "SENTINEL-ATTEST-TOKEN"
	a := &WorkspaceAttestor{ActorUID: "a", Generation: 1, Token: tok}
	requireRedacted(t, "WorkspaceAttestor", "guest.WorkspaceAttestor{redacted}", []string{tok}, a, []*WorkspaceAttestor{a})
}
