package relay

import (
	"fmt"
	"strings"
	"testing"
)

// FEATURE: Relay secrets stay out of every fmt verb (#231).
// SPEC: ClientConfig and Hub print only their redacted form for any verb or
// flag, directly, by pointer and nested in other values.

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

func TestRelaySecretTypesRedactEveryFormatVerb(t *testing.T) {
	const tok, ca = "SENTINEL-RELAY-TOKEN", "SENTINEL-RELAY-CA"
	cfg := ClientConfig{URL: "wss://x", ActorUID: "a", Generation: 1, Token: tok, CAPEM: []byte(ca)}
	hub, err := NewHub([]byte(strings.Repeat("k", 32)), func(string, int64, string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	s := []string{tok, ca}
	requireRedacted(t, "ClientConfig", "relay.ClientConfig{redacted}", s, cfg, &cfg, []ClientConfig{cfg}, map[string]ClientConfig{"k": cfg})
	requireRedacted(t, "Hub", "relay.Hub{redacted}", s, hub, []*Hub{hub})
	type nested struct {
		C ClientConfig
		P *ClientConfig
		H *Hub
	}
	for _, v := range redactVerbs() {
		got := fmt.Sprintf(v, nested{cfg, &cfg, hub})
		for _, x := range s {
			if strings.Contains(got, x) {
				t.Fatalf("nested %s printed a secret: %q", v, got)
			}
		}
	}
}
