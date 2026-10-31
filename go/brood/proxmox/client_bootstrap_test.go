package proxmox

// FEATURE: Rotate bootstrap credentials after OCI import and before each start.
// SPEC: Exact OCI profile only; guard current and pending ownership, pool and
// stopped state; update only env with digest CAS; synchronous null, never a task.
// IMPLEMENTATION: Restricted OCI allowlist, shared env validation, guarded CAS PUT.
// RESULT: IMPLEMENTED — synchronous rotation and fail-closed adversarial cases pass.
// STATUS: DONE

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func ociConfigData() map[string]any {
	d := configData(owner())
	d["entrypoint"] = "/usr/bin/tini -- /usr/local/bin/brood-guest"
	d["cmode"] = "console"
	d["lxc"] = [][]string{{"lxc.init.cwd", "/workspace"}, {"lxc.signal.halt", "SIGTERM"}}
	d["net0"] = d["net0"].(string) + ",host-managed=1"
	return d
}

func TestOCIOwnedLifecycle(t *testing.T) {
	for _, cwd := range []bool{false, true} {
		c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/api2/json/pools":
				envelope(w, poolData())
			case strings.HasSuffix(r.URL.Path, "/config"):
				d := ociConfigData()
				if !cwd {
					d["lxc"] = [][]string{{"lxc.signal.halt", "SIGTERM"}}
				}
				envelope(w, d)
			default:
				envelope(w, task)
			}
		})
		if _, err := c.Start(context.Background(), OwnedCT{123, owner()}); err != nil {
			t.Fatalf("OCI config rejected: %v", err)
		}
	}
}

func TestConfigureBootstrapContract(t *testing.T) {
	var writes atomic.Int32
	var guards []string
	c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			generation := writes.Add(1)
			if r.URL.Path != "/api2/json/nodes/node/lxc/123/config" {
				t.Error("wrong update endpoint")
			}
			_ = r.ParseForm()
			if len(r.PostForm) != 2 || r.PostForm.Get("digest") != strings.Repeat("a", 40) || r.PostForm.Get("env") != "BROOD_GUEST_A=one=two\x00BROOD_GUEST_CA_PEM_B64=Y2E=\x00BROOD_GUEST_TOKEN=guest-private-"+strconv.Itoa(int(generation)) {
				t.Error("update must contain only sorted bootstrap env and checked digest")
			}
			_, _ = io.WriteString(w, `{"data":null}`)
			return
		}
		guards = append(guards, r.URL.RequestURI())
		switch {
		case r.URL.Path == "/api2/json/pools":
			envelope(w, poolData())
		case strings.HasSuffix(r.URL.Path, "/status/current"):
			envelope(w, map[string]any{"status": "stopped"})
		default:
			envelope(w, ociConfigData())
		}
	})
	for generation := 1; generation <= 2; generation++ {
		if err := c.ConfigureBootstrap(context.Background(), OwnedCT{123, owner()}, BootstrapEnv{"BROOD_GUEST_TOKEN": "guest-private-" + strconv.Itoa(generation), "BROOD_GUEST_A": "one=two", "BROOD_GUEST_CA_PEM_B64": "Y2E="}); err != nil {
			t.Fatal(err)
		}
	}
	if writes.Load() != 2 || len(guards) != 8 {
		t.Fatalf("missing per-generation guards: writes %d reads %d", writes.Load(), len(guards))
	}
	if !strings.HasSuffix(guards[0], "/config?current=1") || !strings.HasSuffix(guards[1], "/config") {
		t.Fatal("missing config views")
	}
}

func TestConfigureBootstrapGuards(t *testing.T) {
	for _, drift := range []string{"foreign current", "foreign pending", "pending lxc", "pending rootfs", "pending network", "privileged", "pool", "running", "bad status", "missing digest", "invalid digest", "changed digest", "entrypoint", "cmode", "halt", "cwd", "extra lxc", "duplicate lxc", "malformed lxc", "dotted lxc", "host-managed", "missing host-managed", "missing halt", "legacy lxc"} {
		t.Run(drift, func(t *testing.T) {
			var writes atomic.Int32
			c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					writes.Add(1)
					_, _ = io.WriteString(w, `{"data":null}`)
					return
				}
				switch {
				case r.URL.Path == "/api2/json/pools":
					if drift == "pool" {
						envelope(w, []any{})
						return
					}
					envelope(w, poolData())
				case strings.HasSuffix(r.URL.Path, "/status/current"):
					status := "stopped"
					if drift == "running" {
						status = "running"
					}
					if drift == "bad status" {
						status = secret
					}
					envelope(w, map[string]any{"status": status})
				default:
					d := ociConfigData()
					pending := r.URL.Query().Get("current") != "1"
					switch drift {
					case "foreign current":
						if !pending {
							d["description"] = "foreign"
						}
					case "foreign pending":
						if pending {
							d["description"] = "foreign"
						}
					case "pending lxc":
						if pending {
							d["lxc"] = [][]string{{"lxc.mount.entry", "/dev /dev none bind 0 0"}}
						}
					case "pending rootfs":
						if pending {
							d["rootfs"] = "foreign:subvol-123-disk-0"
						}
					case "pending network":
						if pending {
							d["net0"] = "name=eth0,bridge=other,ip=dhcp"
						}
					case "privileged":
						d["unprivileged"] = 0
					case "missing digest":
						delete(d, "digest")
					case "invalid digest":
						d["digest"] = secret
					case "changed digest":
						if pending {
							d["digest"] = strings.Repeat("b", 40)
						}
					case "entrypoint":
						d["entrypoint"] = "/bin/sh"
					case "cmode":
						d["cmode"] = "shell"
					case "halt":
						d["lxc"] = [][]string{{"lxc.signal.halt", "SIGKILL"}}
					case "cwd":
						d["lxc"] = [][]string{{"lxc.signal.halt", "SIGTERM"}, {"lxc.init.cwd", "/etc"}}
					case "extra lxc":
						d["lxc"] = [][]string{{"lxc.signal.halt", "SIGTERM"}, {"lxc.hook.start", "evil"}}
					case "duplicate lxc":
						d["lxc"] = [][]string{{"lxc.signal.halt", "SIGTERM"}, {"lxc.signal.halt", "SIGTERM"}}
					case "malformed lxc":
						d["lxc"] = [][]string{{"lxc.signal.halt", "SIGTERM", "extra"}}
					case "dotted lxc":
						d["lxc.mount.entry"] = "evil"
					case "host-managed":
						d["net0"] = strings.Replace(d["net0"].(string), "host-managed=1", "host-managed=0", 1)
					case "missing host-managed":
						d["net0"] = strings.TrimSuffix(d["net0"].(string), ",host-managed=1")
					case "missing halt":
						d["lxc"] = [][]string{{"lxc.init.cwd", "/workspace"}}
					case "legacy lxc":
						delete(d, "entrypoint")
					}
					envelope(w, d)
				}
			})
			err := c.ConfigureBootstrap(context.Background(), OwnedCT{123, owner()}, spec().Env)
			if err == nil || writes.Load() != 0 {
				t.Fatalf("unsafe config accepted: %v writes %d", err, writes.Load())
			}
			noLeak(t, err)
		})
	}
}

func TestConfigureBootstrapInputValidation(t *testing.T) {
	var calls atomic.Int32
	c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); envelope(w, task) })
	for _, env := range []BootstrapEnv{nil, {}, {"LD_PRELOAD": "evil"}, {"BROOD_GUEST_TOKEN": secret}, {"BROOD_GUEST_TOKEN": "guest-private\nPEM"}, {"BROOD_GUEST_TOKEN": "nul\x00evil"}, {"BROOD_GUEST_TOKEN": strings.Repeat("a", 8193)}} {
		if err := c.ConfigureBootstrap(context.Background(), OwnedCT{123, owner()}, env); err == nil {
			t.Fatal("accepted invalid bootstrap")
		} else {
			noLeak(t, err)
		}
	}
	if err := c.ConfigureBootstrap(context.Background(), OwnedCT{}, spec().Env); !errors.Is(err, ErrOwnership) {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("invalid input reached upstream")
	}
}

func TestConfigureBootstrapReplySemantics(t *testing.T) {
	for _, reply := range []struct {
		name       string
		status     int
		body       string
		permission bool
	}{
		{"digest mismatch", 500, secret, false},
		{"permission", 403, secret, true},
		{"task not null", 200, `{"data":"` + task + `"}`, false},
		{"malformed secret", 200, `{"data":"guest-private`, false},
		{"error envelope", 200, `{"data":null,"errors":{"env":"guest-private"}}`, false},
		{"missing", 200, `{}`, false},
		{"duplicate", 200, `{"data":null,"data":null}`, false},
		{"trailing", 200, `{"data":null} {}`, false},
	} {
		t.Run(reply.name, func(t *testing.T) {
			var writes atomic.Int32
			c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "PUT" {
					writes.Add(1)
					w.WriteHeader(reply.status)
					_, _ = io.WriteString(w, reply.body)
					return
				}
				switch {
				case r.URL.Path == "/api2/json/pools":
					envelope(w, poolData())
				case strings.HasSuffix(r.URL.Path, "/status/current"):
					envelope(w, map[string]any{"status": "stopped"})
				default:
					envelope(w, ociConfigData())
				}
			})
			err := c.ConfigureBootstrap(context.Background(), OwnedCT{123, owner()}, spec().Env)
			want := ErrUncertain
			if reply.permission {
				want = ErrPermission
			}
			if !errors.Is(err, want) || writes.Load() != 1 {
				t.Fatalf("reply %v writes %d", err, writes.Load())
			}
			noLeak(t, err)
		})
	}
}
