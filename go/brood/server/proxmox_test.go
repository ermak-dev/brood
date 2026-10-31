package server

// FEATURE: Private Proxmox pool configuration and admin-only inventory.
// SPEC: Closed JSON, fail-closed validation, redacted sorted DTOs, no native runtime.
// IMPLEMENTATION: Private typed registry loaded from env; separate redacted admin route.
// RESULT: IMPLEMENTED — strict validation and admin inventory without native runtime.
// STATUS: DONE

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const proxmoxSentinel = "sentinel-private-token-do-not-leak"

func proxmoxFixture(t *testing.T) (map[string]string, map[string]string) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	pool := map[string]string{"endpoint": "https://pve.invalid:8006/", "tokenID": "brood@pve!pool-reader", "node": "node-a", "resourcePool": "brood", "storage": "local-lvm", "network": "vmbr0", "credentialRef": "private-ref", "caPEM": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
	return map[string]string{"BROOD_PROXMOX_POOLS": proxmoxPoolsJSON(t, map[string]any{"brood-proxmox": pool}), "BROOD_PROXMOX_TOKENS": `{"private-ref":"` + proxmoxSentinel + `"}`}, pool
}

func proxmoxPoolsJSON(t *testing.T, value any) string {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func loadProxmox(env map[string]string) (Config, error) {
	return LoadConfig(func(k string) string { return env[k] })
}

func TestProxmoxValidation(t *testing.T) {
	env, base := proxmoxFixture(t)
	if _, err := loadProxmox(env); err != nil {
		t.Fatal(err)
	}
	quoted := map[string]string{"BROOD_PROXMOX_POOLS": "'" + env["BROOD_PROXMOX_POOLS"] + "'", "BROOD_PROXMOX_TOKENS": "'" + env["BROOD_PROXMOX_TOKENS"] + "'"}
	if _, err := loadProxmox(quoted); err != nil {
		t.Fatal(err)
	}
	if _, err := loadProxmox(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := loadProxmox(map[string]string{"BROOD_PROXMOX_POOLS": "{}", "BROOD_PROXMOX_TOKENS": "{}"}); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"https://pve.invalid", "https://pve.invalid:65535", "https://[::1]:8006/"} {
		p := copyProxmoxPool(base)
		p["endpoint"] = endpoint
		good := map[string]string{"BROOD_PROXMOX_POOLS": proxmoxPoolsJSON(t, map[string]any{"pool": p}), "BROOD_PROXMOX_TOKENS": env["BROOD_PROXMOX_TOKENS"]}
		if _, err := loadProxmox(good); err != nil {
			t.Fatalf("valid origin rejected: %v", err)
		}
	}
	cases := []struct{ name, setting, pools, tokens string }{
		{"missing tokens", "BROOD_PROXMOX_TOKENS", env["BROOD_PROXMOX_POOLS"], ""},
		{"missing pools", "BROOD_PROXMOX_POOLS", "", env["BROOD_PROXMOX_TOKENS"]},
	}
	for _, raw := range []string{"null", "[]", `"` + proxmoxSentinel + `"`, "{", `{}` + ` {}`, `{"pool":null}`, `{"pool":[]}`, `{"pool":"` + proxmoxSentinel + `"}`, `{"pool":{},"pool":{}}`, `{"pool":{},"` + string(rune(92)) + `u0070ool":{}}`} {
		cases = append(cases, struct{ name, setting, pools, tokens string }{"bad pools " + fmt.Sprint(len(cases)), "BROOD_PROXMOX_POOLS", raw, env["BROOD_PROXMOX_TOKENS"]})
	}
	for _, raw := range []string{"null", "[]", "{", `{} {}`, `{"private-ref":null}`, `{"private-ref":1}`, `{"private-ref":""}`, `{"private-ref":" "}`, `{"private-ref":"a\rb"}`, `{"private-ref":"a\nb"}`, `{"private-ref":"a\tb"}`, `{"private-ref":"a\u007fb"}`, `{"private-ref":"a\u0000b"}`, `{"private-ref":"x","private-ref":"y"}`, `{"private-ref":{}}`, `{}`, `{"":"x"}`} {
		cases = append(cases, struct{ name, setting, pools, tokens string }{"bad tokens " + fmt.Sprint(len(cases)), "BROOD_PROXMOX_TOKENS", env["BROOD_PROXMOX_POOLS"], raw})
	}
	for _, field := range []string{"endpoint", "tokenID", "node", "resourcePool", "storage", "network", "credentialRef", "caPEM"} {
		for _, value := range []string{"", " ", "a\nb", "a\x00b", "a\x7fb"} {
			p := copyProxmoxPool(base)
			p[field] = value
			cases = append(cases, struct{ name, setting, pools, tokens string }{"invalid " + field + fmt.Sprint(len(cases)), "BROOD_PROXMOX_POOLS", proxmoxPoolsJSON(t, map[string]any{"pool": p}), env["BROOD_PROXMOX_TOKENS"]})
		}
		p := copyProxmoxPool(base)
		delete(p, field)
		cases = append(cases, struct{ name, setting, pools, tokens string }{"missing " + field, "BROOD_PROXMOX_POOLS", proxmoxPoolsJSON(t, map[string]any{"pool": p}), env["BROOD_PROXMOX_TOKENS"]})
	}
	for _, endpoint := range []string{"http://pve.invalid", "https://user:pass@pve.invalid", "https://pve.invalid/api2/json", "https://pve.invalid//", "https://pve.invalid?", "https://pve.invalid?q=x", "https://pve.invalid#", "https://pve.invalid#x", "https:pve.invalid", "https:///", "https://pve.invalid:0", "https://pve.invalid:65536", "https://pve.invalid:abc", "https://pve.invalid:", "https://pve.invalid/%2f", "https://pve.invalid\\x", "https://[not-an-ip]:8006", "https://a:b:8006"} {
		p := copyProxmoxPool(base)
		p["endpoint"] = endpoint
		cases = append(cases, struct{ name, setting, pools, tokens string }{"endpoint " + fmt.Sprint(len(cases)), "BROOD_PROXMOX_POOLS", proxmoxPoolsJSON(t, map[string]any{"pool": p}), env["BROOD_PROXMOX_TOKENS"]})
	}
	for _, tokenID := range []string{"root", "user@realm", "user!token", "user@realm!", "@realm!token", "user@!token", "user@realm!token=secret", "user@realm!token extra", "user@realm!a!b", "user@realm!a:b"} {
		p := copyProxmoxPool(base)
		p["tokenID"] = tokenID
		cases = append(cases, struct{ name, setting, pools, tokens string }{"tokenID " + fmt.Sprint(len(cases)), "BROOD_PROXMOX_POOLS", proxmoxPoolsJSON(t, map[string]any{"pool": p}), env["BROOD_PROXMOX_TOKENS"]})
	}
	for _, ca := range []string{proxmoxSentinel, base["caPEM"] + proxmoxSentinel, strings.Replace(base["caPEM"], "CERTIFICATE", "PRIVATE KEY", -1), "-----BEGIN CERTIFICATE-----\nYWJj\n-----END CERTIFICATE-----\n", "-----BEGIN CERTIFICATE-----\nmalformed\n" + base["caPEM"]} {
		p := copyProxmoxPool(base)
		p["caPEM"] = ca
		cases = append(cases, struct{ name, setting, pools, tokens string }{"caPEM " + fmt.Sprint(len(cases)), "BROOD_PROXMOX_POOLS", proxmoxPoolsJSON(t, map[string]any{"pool": p}), env["BROOD_PROXMOX_TOKENS"]})
	}
	p := copyProxmoxPool(base)
	p[proxmoxSentinel] = proxmoxSentinel
	cases = append(cases, struct{ name, setting, pools, tokens string }{"unknown field", "BROOD_PROXMOX_POOLS", proxmoxPoolsJSON(t, map[string]any{"pool": p}), env["BROOD_PROXMOX_TOKENS"]})
	for _, name := range []string{"", " ", "bad\nname"} {
		cases = append(cases, struct{ name, setting, pools, tokens string }{"pool key " + fmt.Sprint(len(cases)), "BROOD_PROXMOX_POOLS", proxmoxPoolsJSON(t, map[string]any{name: base}), env["BROOD_PROXMOX_TOKENS"]})
	}
	duplicate := strings.Replace(env["BROOD_PROXMOX_POOLS"], `"node":`, `"node":"hidden","node":`, 1)
	cases = append(cases, struct{ name, setting, pools, tokens string }{"nested duplicate", "BROOD_PROXMOX_POOLS", duplicate, env["BROOD_PROXMOX_TOKENS"]})
	invalidUTF8 := strings.Replace(env["BROOD_PROXMOX_POOLS"], "node-a", string([]byte{0xff}), 1)
	cases = append(cases, struct{ name, setting, pools, tokens string }{"invalid UTF-8", "BROOD_PROXMOX_POOLS", invalidUTF8, env["BROOD_PROXMOX_TOKENS"]})
	for _, field := range []string{"endpoint", "tokenID", "node", "resourcePool", "storage", "network", "credentialRef", "caPEM"} {
		for _, value := range []any{nil, true, 1, []any{}, map[string]any{}} {
			p := map[string]any{}
			for k, v := range base {
				p[k] = v
			}
			p[field] = value
			cases = append(cases, struct{ name, setting, pools, tokens string }{"wrong field type " + field + fmt.Sprint(len(cases)), "BROOD_PROXMOX_POOLS", proxmoxPoolsJSON(t, map[string]any{"pool": p}), env["BROOD_PROXMOX_TOKENS"]})
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadProxmox(map[string]string{"BROOD_PROXMOX_POOLS": tc.pools, "BROOD_PROXMOX_TOKENS": tc.tokens})
			if err == nil {
				t.Fatal("invalid configuration accepted")
			}
			var ce *ConfigError
			if !errors.As(err, &ce) || ce.Setting != tc.setting {
				t.Fatalf("expected setting %s, got %v", tc.setting, err)
			}
			if strings.Contains(err.Error(), proxmoxSentinel) || strings.Contains(err.Error(), "private-ref") || strings.Contains(err.Error(), "hidden") {
				t.Fatal("submitted value leaked in error")
			}
		})
	}
}

func copyProxmoxPool(p map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range p {
		out[k] = v
	}
	return out
}

func TestProxmoxAPI(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(fmt.Sprint(configured), func(t *testing.T) {
			env, pool := proxmoxFixture(t)
			var upstreamRequests atomic.Int32
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamRequests.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			t.Cleanup(upstream.Close)
			pool["endpoint"] = upstream.URL
			if configured {
				env["BROOD_PROXMOX_POOLS"] = proxmoxPoolsJSON(t, map[string]any{"z-pool": pool, "brood-proxmox": pool})
			} else {
				env = nil
			}
			cfg, err := loadProxmox(env)
			if err != nil {
				t.Fatal(err)
			}
			serialized, _ := json.Marshal(cfg)
			if strings.Contains(string(serialized), proxmoxSentinel) || strings.Contains(string(serialized), "caPEM") {
				t.Fatal("private registry exported by Config JSON")
			}
			cfg.DBPath = t.TempDir() + "/brood.db"
			srv, err := New(nil, cfg, Options{})
			if err != nil {
				t.Fatal(err)
			}
			ts := &testServer{t: t, srv: srv, http: httptest.NewServer(srv.Handler())}
			t.Cleanup(func() { ts.http.Close(); srv.Close() })
			anon := newClient(t)
			resp, data, _ := ts.do(anon, "GET", "/api/proxmox-pools", "", nil)
			expectStatus(t, "anonymous", resp, data, http.StatusUnauthorized)
			ts.bootstrapAdmin("admin", "admin-password-1")
			admin := newClient(t)
			ts.login(admin, "admin", "admin-password-1")
			resp, data, _ = ts.do(admin, "POST", "/api/admin/users", ts.csrf(admin), map[string]string{"username": "ordinary", "password": "ordinary-password-1"})
			expectStatus(t, "create ordinary", resp, data, http.StatusCreated)
			ordinary := newClient(t)
			ts.login(ordinary, "ordinary", "ordinary-password-1")
			resp, data, _ = ts.do(ordinary, "GET", "/api/proxmox-pools", "", nil)
			expectStatus(t, "ordinary", resp, data, http.StatusForbidden)
			resp, data, raw := ts.do(admin, "GET", "/api/proxmox-pools", "", nil)
			expectStatus(t, "admin without Kubernetes", resp, data, http.StatusOK)
			if strings.Contains(raw, proxmoxSentinel) || strings.Contains(raw, "private-ref") || strings.Contains(raw, "caPEM") || strings.Contains(raw, "credentialRef") {
				t.Fatal("private data leaked")
			}
			want := []any{}
			if configured {
				for _, name := range []string{"brood-proxmox", "z-pool"} {
					want = append(want, map[string]any{"name": name, "endpoint": pool["endpoint"], "tokenID": pool["tokenID"], "node": pool["node"], "resourcePool": pool["resourcePool"], "storage": pool["storage"], "network": pool["network"], "credentialConfigured": true, "runtimeAvailable": false})
				}
			}
			if !reflect.DeepEqual(data, map[string]any{"items": want}) {
				t.Fatalf("unexpected response: %s", raw)
			}
			if srv.pools != nil || srv.ateapi != nil {
				t.Fatal("logical pool synthesized native client")
			}
			resp, data, _ = ts.do(admin, "GET", "/api/worker-pools", "", nil)
			expectStatus(t, "native pools remain unconfigured", resp, data, http.StatusServiceUnavailable)
			if upstreamRequests.Load() != 0 {
				t.Fatal("inventory contacted Proxmox upstream")
			}
		})
	}
}
