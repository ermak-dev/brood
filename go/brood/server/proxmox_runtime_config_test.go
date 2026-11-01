package server

// FEATURE: Optional approved Proxmox runtime configuration.
// SPEC: Legacy connections remain readable; runtime configuration is closed,
// complete, positive, digest-pinned, and never exposes installation secrets.

import (
	"encoding/json"
	"strings"
	"testing"
)

func runtimeFixture(t *testing.T) map[string]string {
	t.Helper()
	env, base := proxmoxFixture(t)
	p := map[string]any{}
	for k, v := range base {
		p[k] = v
	}
	p["template"] = "local:vztmpl/brood-claude-proxmox-aaaaaaaa.tar.zst"
	p["templateDigest"] = "sha256:" + strings.Repeat("a", 64)
	p["capacity"] = map[string]any{"actors": 2, "cpuMillicores": "2000", "memoryBytes": "2147483648", "diskGiB": 8}
	env[proxmoxPoolsSetting] = proxmoxPoolsJSON(t, map[string]any{"brood-proxmox": p})
	env["BROOD_PUBLIC_ORIGIN"] = "https://brood.invalid"
	env["BROOD_PROXMOX_RELAY_SECRET"] = strings.Repeat("r", 32)
	return env
}

func TestProxmoxRuntimeConfiguration(t *testing.T) {
	env := runtimeFixture(t)
	cfg, err := loadProxmox(env)
	if err != nil {
		t.Fatalf("approved runtime configuration rejected: %v", err)
	}
	items := cfg.proxmox.items()
	if len(items) != 1 || !items[0].RuntimeAvailable {
		t.Fatal("installed runtime is unavailable")
	}
	raw, _ := json.Marshal(cfg)
	if strings.Contains(string(raw), strings.Repeat("r", 32)) {
		t.Fatal("relay secret exported")
	}
	for _, setting := range []string{"BROOD_PUBLIC_ORIGIN", "BROOD_PROXMOX_RELAY_SECRET"} {
		t.Run("missing "+setting, func(t *testing.T) {
			e := runtimeFixture(t)
			delete(e, setting)
			c, err := loadProxmox(e)
			if err != nil {
				t.Fatal(err)
			}
			if c.proxmox.items()[0].RuntimeAvailable {
				t.Fatal("runtime activated without installation prerequisites")
			}
		})
	}
}

func TestProxmoxRuntimePoolNames(t *testing.T) {
	for _, name := range []string{"pool with spaces", "../pool", "Uppercase", strings.Repeat("x", 64)} {
		e := runtimeFixture(t)
		var pools map[string]any
		json.Unmarshal([]byte(e[proxmoxPoolsSetting]), &pools)
		e[proxmoxPoolsSetting] = proxmoxPoolsJSON(t, map[string]any{name: pools["brood-proxmox"]})
		if _, err := loadProxmox(e); err == nil {
			t.Fatalf("unsafe runtime pool name accepted: %q", name)
		}
	}
}

func TestProxmoxRuntimeConfigurationRejectsPartialAndUnsafe(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"partial", func(p map[string]any) { delete(p, "templateDigest") }},
		{"OCI instead of template", func(p map[string]any) { p["template"] = "docker.io/arbitrary:latest" }},
		{"foreign template", func(p map[string]any) { p["template"] = "local:vztmpl/foreign-template.tar.zst" }},
		{"disk overflow", func(p map[string]any) { p["capacity"].(map[string]any)["diskGiB"] = int64(9223372036854775807) }},
		{"insufficient core shares", func(p map[string]any) { p["capacity"].(map[string]any)["cpuMillicores"] = "1000" }},
		{"insufficient disk shares", func(p map[string]any) { p["capacity"].(map[string]any)["diskGiB"] = 1 }},
		{"insufficient memory shares", func(p map[string]any) { p["capacity"].(map[string]any)["memoryBytes"] = "1024" }},
		{"unpinned", func(p map[string]any) { p["templateDigest"] = "latest" }},
		{"capacity unknown", func(p map[string]any) { p["capacity"].(map[string]any)["extra"] = 1 }},
		{"zero actors", func(p map[string]any) { p["capacity"].(map[string]any)["actors"] = 0 }},
		{"numeric CPU", func(p map[string]any) { p["capacity"].(map[string]any)["cpuMillicores"] = 2000 }},
		{"overflow memory", func(p map[string]any) { p["capacity"].(map[string]any)["memoryBytes"] = "9223372036854775808" }},
		{"zero disk", func(p map[string]any) { p["capacity"].(map[string]any)["diskGiB"] = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := runtimeFixture(t)
			var pools map[string]any
			json.Unmarshal([]byte(e[proxmoxPoolsSetting]), &pools)
			tc.change(pools["brood-proxmox"].(map[string]any))
			e[proxmoxPoolsSetting] = proxmoxPoolsJSON(t, pools)
			if _, err := loadProxmox(e); err == nil {
				t.Fatal("unsafe runtime configuration accepted")
			}
		})
	}
	for _, tc := range []struct{ setting, value string }{{"BROOD_PUBLIC_ORIGIN", "http://brood.invalid"}, {"BROOD_PUBLIC_ORIGIN", "https://brood.invalid/path"}, {"BROOD_PROXMOX_RELAY_SECRET", "short"}} {
		t.Run(tc.setting+tc.value, func(t *testing.T) {
			e := runtimeFixture(t)
			e[tc.setting] = tc.value
			if _, err := loadProxmox(e); err == nil {
				t.Fatal("invalid installation setting accepted")
			}
		})
	}
}
