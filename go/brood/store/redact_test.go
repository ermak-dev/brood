package store

import (
	"fmt"
	"strings"
	"testing"
)

// FEATURE: Managed connection secrets stay out of every fmt verb (#231).
// SPEC: ManagedConnection prints only its redacted form for any verb or flag,
// directly, by pointer and nested in other values.

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

func TestManagedConnectionRedactsEveryFormatVerb(t *testing.T) {
	const tok, ca = "SENTINEL-MC-TOKEN", "SENTINEL-MC-CA"
	c := ManagedConnection{UID: "u", Name: "n", TokenSecret: tok, CAPEM: ca}
	s := []string{tok, ca}
	requireRedacted(t, "ManagedConnection", "ManagedConnection{redacted}", s, c, &c, []ManagedConnection{c}, map[string]ManagedConnection{"k": c})
	type nested struct {
		C ManagedConnection
		P *ManagedConnection
	}
	for _, v := range redactVerbs() {
		got := fmt.Sprintf(v, nested{c, &c})
		for _, x := range s {
			if strings.Contains(got, x) {
				t.Fatalf("nested %s printed a secret: %q", v, got)
			}
		}
	}
}
