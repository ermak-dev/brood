package proxmox

// FEATURE: Scoped HTTPS PVE client for strictly owned unprivileged containers.
// SPEC: Verified TLS, no redirects, bounded/redacted envelopes, full ownership
// and live pool membership before writes, explicit templates, and task uncertainty.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const secret = "sentinel-private-pve-token"
const task = "UPID:node:00000001:00000002:00000003:vzcreate:123:user@pve!brood:"

func owner() Ownership {
	return Ownership{PoolUID: "pool-uid", ActorUID: "actor-uid", Incarnation: 1, Nonce: "nonce-unique"}
}
func spec() CTSpec {
	return CTSpec{VMID: 123, Hostname: "actor", Template: "templates:vztmpl/brood-rootfs.tar.zst", Owner: owner(), Cores: 2, MemoryMiB: 512, DiskGiB: 8, MAC: "02:00:00:00:00:01", Env: BootstrapEnv{"BROOD_GUEST_TOKEN": "guest-private"}}
}
func fixture(t *testing.T, h http.HandlerFunc) (*Client, Config) {
	t.Helper()
	s := httptest.NewTLSServer(h)
	t.Cleanup(s.Close)
	cfg := Config{Endpoint: s.URL, TokenID: "user@pve!brood", TokenSecret: secret, CAPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw})), Node: "node", ResourcePool: "actors", Storage: "disks", Network: "vmbr1", TemplateStorage: "templates", TemplatePrefix: "brood-"}
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c, cfg
}
func envelope(w http.ResponseWriter, data any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}
func configData(o Ownership) map[string]any {
	return map[string]any{"description": OwnedDescription(o), "unprivileged": 1, "rootfs": "disks:subvol-123-disk-0,size=8G", "digest": strings.Repeat("a", 40), "hostname": "actor", "net0": "name=eth0,bridge=vmbr1,ip=dhcp,hwaddr=02:00:00:00:00:01", "env": secret, "lxc.environment.runtime": secret}
}
func poolData() any {
	return []any{map[string]any{"poolid": "actors", "members": []any{map[string]any{"type": "lxc", "node": "node", "vmid": 123}}}}
}
func noLeak(t *testing.T, values ...any) {
	t.Helper()
	for _, v := range values {
		for _, s := range []string{fmt.Sprint(v), fmt.Sprintf("%+v", v), fmt.Sprintf("%#v", v)} {
			if strings.Contains(s, secret) || strings.Contains(s, "guest-private") {
				t.Fatalf("secret exposed")
			}
		}
		b, _ := json.Marshal(v)
		if strings.Contains(string(b), secret) || strings.Contains(string(b), "guest-private") {
			t.Fatal("JSON secret exposed")
		}
	}
}

func TestConfigAndSecrets(t *testing.T) {
	_, cfg := fixture(t, func(w http.ResponseWriter, r *http.Request) { envelope(w, "123") })
	noLeak(t, cfg, spec(), spec().Env)
	for _, change := range []func(*Config){func(c *Config) { c.Endpoint = "http://example.test" }, func(c *Config) { c.Endpoint += "/evil" }, func(c *Config) { c.Endpoint = "https://user:pass@example.test" }, func(c *Config) { c.TokenSecret = "bad\r\nheader" }, func(c *Config) { c.Node = "../node" }, func(c *Config) { c.Storage = "" }, func(c *Config) { c.TemplateStorage = "" }, func(c *Config) { c.TemplatePrefix = "../" }, func(c *Config) { c.CAPEM = "invalid" }} {
		bad := cfg
		change(&bad)
		if _, err := NewClient(bad); err == nil {
			t.Fatal("accepted invalid config")
		}
	}
}
func TestVerifiedTLS(t *testing.T) {
	c, cfg := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/cluster/nextid" || r.Header.Get("Authorization") != "PVEAPIToken=user@pve!brood="+secret {
			t.Error("wrong request")
		}
		envelope(w, "123")
	})
	id, err := c.NextID(context.Background())
	if err != nil || id != 123 {
		t.Fatalf("NextID = %d, %v", id, err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "unrelated test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	bad := cfg
	bad.CAPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	bc, err := NewClient(bad)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = bc.NextID(context.Background()); err == nil {
		t.Fatal("accepted untrusted CA")
	}
	noLeak(t, err)
	bad = cfg
	u, _ := url.Parse(cfg.Endpoint)
	u.Host = "localhost:" + u.Port()
	bad.Endpoint = u.String()
	bc, err = NewClient(bad)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = bc.NextID(context.Background()); err == nil {
		t.Fatal("accepted wrong hostname")
	}
}
func TestRedirectAndErrors(t *testing.T) {
	var leaked atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer target.Close()
	for _, status := range []int{302, 401, 403, 409, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", target.URL+"/"+secret)
				w.WriteHeader(status)
				_, _ = io.WriteString(w, secret)
			})
			_, err := c.NextID(context.Background())
			if err == nil {
				t.Fatal("accepted error")
			}
			noLeak(t, err)
			if (status == 401 || status == 403) && !errors.Is(err, ErrPermission) {
				t.Fatal("missing permission classification")
			}
		})
	}
	if leaked.Load() != 0 {
		t.Fatal("followed redirect")
	}
}
func TestMalformedBoundedEnvelopes(t *testing.T) {
	for _, body := range []string{"{", `{"data":null}`, `{"data":"not-an-id"}`, `{"data":"123"} {}`, `{"errors":{"secret":"` + secret + `"},"data":"123"}`, strings.Repeat("x", (1<<20)+1)} {
		c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) })
		_, err := c.NextID(context.Background())
		if err == nil {
			t.Fatalf("accepted malformed envelope size %d", len(body))
		}
		noLeak(t, err)
	}
}
func TestCreateContract(t *testing.T) {
	var writes int
	c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		writes++
		if r.Method != "POST" || r.URL.Path != "/api2/json/nodes/node/lxc" {
			t.Error("wrong create endpoint")
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		f := r.PostForm
		for k, v := range map[string]string{"vmid": "123", "pool": "actors", "ostemplate": spec().Template, "unprivileged": "1", "rootfs": "disks:8", "cores": "2", "memory": "512", "swap": "0", "start": "0", "description": OwnedDescription(owner()), "net0": "name=eth0,bridge=vmbr1,ip=dhcp,hwaddr=02:00:00:00:00:01", "env": "BROOD_GUEST_TOKEN=guest-private"} {
			if f.Get(k) != v {
				t.Errorf("wrong %s", k)
			}
		}
		for _, k := range []string{"tags", "force", "purge", "hookscript", "mp0", "dev0", "privileged", "features", "password"} {
			if f.Has(k) {
				t.Errorf("unsafe create field %s", k)
			}
		}
		if strings.Contains(f.Get("env"), secret) {
			t.Error("credential in guest")
		}
		envelope(w, task)
	})
	got, err := c.Create(context.Background(), spec())
	if err != nil || got != task || writes != 1 {
		t.Fatalf("Create = %q, %v, writes %d", got, err, writes)
	}
}
func TestCreateValidation(t *testing.T) {
	var writes atomic.Int32
	c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) { writes.Add(1); envelope(w, task) })
	for _, change := range []func(*CTSpec){func(s *CTSpec) { s.VMID = 0 }, func(s *CTSpec) { s.Cores = 0 }, func(s *CTSpec) { s.Cores = 8192 }, func(s *CTSpec) { s.MemoryMiB = -1 }, func(s *CTSpec) { s.MemoryMiB = 1 << 30 }, func(s *CTSpec) { s.DiskGiB = 0 }, func(s *CTSpec) { s.DiskGiB = 1 << 30 }, func(s *CTSpec) { s.Owner.Nonce = "" }, func(s *CTSpec) { s.Owner.Incarnation = 0 }, func(s *CTSpec) { s.Template = "other:vztmpl/brood-rootfs.tar.zst" }, func(s *CTSpec) { s.Template = "templates:vztmpl/foreign.tar.zst" }, func(s *CTSpec) { s.Template = "templates:vztmpl/brood-../escape" }, func(s *CTSpec) { s.Hostname = "a,hookscript=x" }, func(s *CTSpec) { s.MAC = "01:00:00:00:00:01" }, func(s *CTSpec) { s.Env = BootstrapEnv{"BAD\x00KEY": "value"} }, func(s *CTSpec) { s.Env = BootstrapEnv{"BROOD_GUEST_TOKEN": secret} }, func(s *CTSpec) { s.Env = BootstrapEnv{"LD_PRELOAD": "/evil"} }} {
		s := spec()
		change(&s)
		if _, err := c.Create(context.Background(), s); err == nil {
			t.Fatal("accepted unsafe spec")
		}
	}
	if writes.Load() != 0 {
		t.Fatal("validation wrote to PVE")
	}
}
func TestOwnedLifecycle(t *testing.T) {
	for _, op := range []string{"start", "shutdown", "stop", "delete"} {
		t.Run(op, func(t *testing.T) {
			var paths []string
			c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.Method+" "+r.URL.RequestURI())
				switch {
				case r.URL.Path == "/api2/json/pools":
					envelope(w, poolData())
				case strings.HasSuffix(r.URL.Path, "/config"):
					envelope(w, configData(owner()))
				default:
					if r.URL.RawQuery != "" {
						t.Error("unexpected mutation flags")
					}
					_ = r.ParseForm()
					if len(r.PostForm) != 0 {
						t.Error("unsupported mutation parameters")
					}
					envelope(w, task)
				}
			})
			ref := OwnedCT{VMID: 123, Owner: owner()}
			var got string
			var err error
			switch op {
			case "start":
				got, err = c.Start(context.Background(), ref)
			case "shutdown":
				got, err = c.Shutdown(context.Background(), ref)
			case "stop":
				got, err = c.Stop(context.Background(), ref)
			case "delete":
				got, err = c.Delete(context.Background(), ref)
			}
			if err != nil || got != task {
				t.Fatalf("%s: %v", op, err)
			}
			if len(paths) != 4 || !strings.HasSuffix(paths[0], "/config?current=1") || !strings.HasSuffix(paths[1], "/config") || !strings.Contains(paths[2], "poolid=actors") {
				t.Fatalf("missing live guards: %v", paths)
			}
			want := "POST /api2/json/nodes/node/lxc/123/status/" + op
			if op == "delete" {
				want = "DELETE /api2/json/nodes/node/lxc/123"
			}
			if paths[3] != want {
				t.Fatalf("write %s", paths[3])
			}
		})
	}
}
func TestOwnershipGuards(t *testing.T) {
	for _, which := range []string{"foreign", "pool", "stale", "nonce", "privileged", "storage", "missing", "node", "type"} {
		for _, op := range []string{"start", "shutdown", "stop", "delete"} {
			t.Run(which+"/"+op, func(t *testing.T) {
				var writes atomic.Int32
				c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
					if r.Method != "GET" {
						writes.Add(1)
						envelope(w, task)
						return
					}
					if r.URL.Path == "/api2/json/pools" {
						p := poolData()
						if which == "pool" {
							p = []any{map[string]any{"poolid": "foreign", "members": []any{map[string]any{"type": "lxc", "node": "node", "vmid": 123}}}}
						}
						if which == "node" || which == "type" {
							m := p.([]any)[0].(map[string]any)["members"].([]any)[0].(map[string]any)
							m[which] = "foreign"
						}
						envelope(w, p)
						return
					}
					o := owner()
					switch which {
					case "stale":
						o.Incarnation++
					case "nonce":
						o.Nonce = "different"
					case "foreign":
						o.ActorUID = "another"
					}
					d := configData(o)
					if which == "privileged" {
						d["unprivileged"] = 0
					}
					if which == "storage" {
						d["rootfs"] = "foreign:subvol-123-disk-0"
					}
					if which == "missing" {
						delete(d, "description")
					}
					envelope(w, d)
				})
				ref := OwnedCT{VMID: 123, Owner: owner()}
				var err error
				switch op {
				case "start":
					_, err = c.Start(context.Background(), ref)
				case "shutdown":
					_, err = c.Shutdown(context.Background(), ref)
				case "stop":
					_, err = c.Stop(context.Background(), ref)
				case "delete":
					_, err = c.Delete(context.Background(), ref)
				}
				if !errors.Is(err, ErrOwnership) || writes.Load() != 0 {
					t.Fatalf("guard %v, writes %d", err, writes.Load())
				}
			})
		}
	}
}
func TestSafeReads(t *testing.T) {
	c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/config") {
			envelope(w, configData(owner()))
		} else {
			envelope(w, map[string]any{"status": "running", "env": secret})
		}
	})
	cfg, err := c.GetConfig(context.Background(), 123)
	if err != nil {
		t.Fatal(err)
	}
	noLeak(t, cfg)
	if cfg.Description != OwnedDescription(owner()) {
		t.Fatal("description lost")
	}
	st, err := c.Status(context.Background(), 123)
	if err != nil || st.Status != "running" {
		t.Fatalf("status %v %v", st, err)
	}
	noLeak(t, st)
}
func TestTasks(t *testing.T) {
	for _, state := range []string{"OK", "failed " + secret, "missing", "running", "malformed"} {
		t.Run(state, func(t *testing.T) {
			c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/status") || !strings.Contains(r.URL.Path, "/tasks/UPID:") {
					t.Error("wrong task path")
				}
				d := map[string]any{"status": "stopped"}
				switch state {
				case "missing":
				case "running":
					d["status"] = "running"
				case "malformed":
					d["status"] = "unknown"
				default:
					d["exitstatus"] = state
				}
				envelope(w, d)
			})
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			err := c.WaitTask(ctx, task)
			if state == "OK" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if err == nil {
					t.Fatal("accepted incomplete task")
				}
				if state == "failed "+secret && !errors.Is(err, ErrTaskFailed) {
					t.Fatal(err)
				}
				if state != "failed "+secret && !errors.Is(err, ErrUncertain) {
					t.Fatal(err)
				}
				noLeak(t, err)
			}
			if state == "running" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("lost cancellation")
			}
		})
	}
}
func TestUncertainAndCollisionNoRetry(t *testing.T) {
	for _, status := range []int{409, 500, 200} {
		var calls atomic.Int32
		c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"data":null,"secret":"`+secret+`"}`)
		})
		_, err := c.Create(context.Background(), spec())
		if err == nil || calls.Load() != 1 {
			t.Fatal("create retried or succeeded")
		}
		if status == 409 && !errors.Is(err, ErrCollision) {
			t.Fatal(err)
		}
		if status != 409 && !errors.Is(err, ErrUncertain) {
			t.Fatal(err)
		}
		noLeak(t, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("canceled request reached server") })
	_, err := c.Create(ctx, spec())
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
