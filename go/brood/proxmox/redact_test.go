package proxmox

import (
	"fmt"
	"strings"
	"testing"
)

// FEATURE: Proxmox secrets stay out of every fmt verb (#231).
// SPEC: Config, BootstrapEnv, CTSpec and Client print only their redacted
// form for any verb or flag, directly, by pointer and nested in other values.

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

func TestSecretTypesRedactEveryFormatVerb(t *testing.T) {
	const tok, ca, env = "SENTINEL-TOKEN", "SENTINEL-CA", "SENTINEL-ENV"
	cfg := Config{Endpoint: "https://pve.example:8006", TokenID: "u@pam!t", TokenSecret: tok, CAPEM: ca, Node: "n"}
	envs := BootstrapEnv{"BROOD_TOKEN": env}
	spec := CTSpec{VMID: 100, Hostname: "h", Env: envs}
	client := &Client{cfg: cfg}
	s := []string{tok, ca, env}
	type nested struct {
		C   Config
		P   *Config
		E   BootstrapEnv
		S   CTSpec
		Cl  *Client
		Any any
	}
	n := nested{cfg, &cfg, envs, spec, client, spec}
	requireRedacted(t, "Config", "proxmox.Config{redacted}", s, cfg, &cfg, []Config{cfg}, map[string]Config{"k": cfg})
	requireRedacted(t, "BootstrapEnv", "proxmox.BootstrapEnv{redacted}", s, envs, &envs, []BootstrapEnv{envs})
	requireRedacted(t, "CTSpec", "proxmox.CTSpec{redacted}", s, spec, &spec, []CTSpec{spec})
	requireRedacted(t, "Client", "proxmox.Client{redacted}", s, client, []*Client{client})
	for _, v := range redactVerbs() {
		got := fmt.Sprintf(v, n)
		for _, x := range s {
			if strings.Contains(got, x) {
				t.Fatalf("nested %s printed a secret: %q", v, got)
			}
		}
	}
}
