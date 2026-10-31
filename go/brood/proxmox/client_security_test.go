package proxmox

// FEATURE: Reject live configuration drift and prevent partially decoded leaks.
// SPEC: Mutation guards cover the configured network and reject additional NICs;
// public read failures return empty data, and authorization never uses a cache.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNetworkDriftBeforeWrites(t *testing.T) {
	for _, drift := range []string{"foreign bridge", "static IP", "extra NIC", "missing NIC", "duplicate bridge", "unknown setting"} {
		for _, op := range []string{"start", "shutdown", "stop", "delete"} {
			t.Run(drift+"/"+op, func(t *testing.T) {
				var writes atomic.Int32
				c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
					if r.Method != "GET" {
						writes.Add(1)
						envelope(w, task)
						return
					}
					if r.URL.Path == "/api2/json/pools" {
						envelope(w, poolData())
						return
					}
					data := configData(owner())
					switch drift {
					case "foreign bridge":
						data["net0"] = "name=eth0,bridge=other,ip=dhcp"
					case "static IP":
						data["net0"] = "name=eth0,bridge=vmbr1,ip=10.0.0.2/24"
					case "extra NIC":
						data["net1"] = "name=eth1,bridge=other,ip=dhcp"
					case "missing NIC":
						delete(data, "net0")
					case "duplicate bridge":
						data["net0"] = "name=eth0,bridge=vmbr1,bridge=other,ip=dhcp"
					case "unknown setting":
						data["net0"] = "name=eth0,bridge=vmbr1,ip=dhcp,tag=42"
					}
					envelope(w, data)
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
					t.Fatalf("network drift accepted: %v, writes %d", err, writes.Load())
				}
			})
		}
	}
}

func TestReadFailureDoesNotReturnPartialStatus(t *testing.T) {
	c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"status":"`+secret+`","Status":3}}`)
	})
	status, err := c.Status(context.Background(), 123)
	if err == nil {
		t.Fatal("accepted invalid status")
	}
	noLeak(t, status, err)
	if status != (CTStatus{}) {
		t.Fatal("returned partial status")
	}
}

func TestOwnershipRecheckedEveryTime(t *testing.T) {
	var writes atomic.Int32
	c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			writes.Add(1)
			envelope(w, task)
			return
		}
		if r.URL.Path == "/api2/json/pools" {
			envelope(w, poolData())
			return
		}
		o := owner()
		if writes.Load() > 0 {
			o.Incarnation++
		}
		envelope(w, configData(o))
	})
	ref := OwnedCT{VMID: 123, Owner: owner()}
	if _, err := c.Start(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Delete(context.Background(), ref); !errors.Is(err, ErrOwnership) {
		t.Fatal("cached ownership")
	}
	if writes.Load() != 1 {
		t.Fatal("wrote after ownership changed")
	}
}

func TestBootstrapEncodingAndStableMAC(t *testing.T) {
	var nets []string
	c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		nets = append(nets, r.PostForm.Get("net0"))
		if r.PostForm.Get("env") != "BROOD_GUEST_A=one=two\x00BROOD_GUEST_Z=hello world" {
			t.Error("environment is not sorted NUL-separated assignments")
		}
		envelope(w, task)
	})
	s := spec()
	s.MAC = ""
	s.Env = BootstrapEnv{"BROOD_GUEST_Z": "hello world", "BROOD_GUEST_A": "one=two"}
	for range 2 {
		if _, err := c.Create(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
	s.Owner.Incarnation++
	if _, err := c.Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if nets[0] != nets[1] || nets[0] == nets[2] {
		t.Fatal("MAC does not follow stable full identity")
	}
}

func TestTaskPollingAndCancellation(t *testing.T) {
	var polls atomic.Int32
	c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if polls.Add(1) == 1 {
			envelope(w, map[string]any{"status": "running"})
			return
		}
		envelope(w, map[string]any{"status": "stopped", "exitstatus": "OK"})
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.WaitTask(ctx, task); err != nil || polls.Load() != 2 {
		t.Fatalf("polling %v", err)
	}
	for _, id := range []string{"", strings.Replace(task, "UPID:node:", "UPID:foreign:", 1), task + "/../../secret"} {
		before := polls.Load()
		if err := c.WaitTask(ctx, id); !errors.Is(err, ErrUncertain) {
			t.Fatal("accepted invalid task")
		}
		if polls.Load() != before {
			t.Fatal("invalid task reached upstream")
		}
	}
}

func TestAbruptWriteResponseIsUncertain(t *testing.T) {
	var writes atomic.Int32
	c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		writes.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	})
	_, err := c.Create(context.Background(), spec())
	if !errors.Is(err, ErrUncertain) || writes.Load() != 1 {
		t.Fatalf("lost response %v, writes %d", err, writes.Load())
	}
	noLeak(t, err)
}

func TestRedirectSameOriginNeverResubmitsToken(t *testing.T) {
	var second atomic.Int32
	c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token-target" {
			second.Add(1)
			envelope(w, "123")
			return
		}
		w.Header().Set("Location", "/token-target")
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	if _, err := c.NextID(context.Background()); err == nil {
		t.Fatal("followed same-origin redirect")
	}
	if second.Load() != 0 {
		t.Fatal("token sent to redirect target")
	}
}

func TestStrictEnvelopeAndVMIDBoundaries(t *testing.T) {
	for _, body := range []string{`{"data":"123","data":"124"}`, `[]`, `{"data":99}`, `{"data":1000000000}`, `{"data":"+123"}`} {
		c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) })
		if _, err := c.NextID(context.Background()); err == nil {
			t.Fatalf("accepted invalid response %s", body)
		}
	}
	for _, id := range []int{100, 999999999} {
		c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) { envelope(w, id) })
		if got, err := c.NextID(context.Background()); err != nil || got != id {
			t.Fatalf("boundary %d %v", id, err)
		}
	}
}

func TestPVEConfigRoundTrip(t *testing.T) {
	c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api2/json/pools":
			envelope(w, poolData())
		case strings.HasSuffix(r.URL.Path, "/config"):
			d := configData(owner())
			// PVE writes description as comments, then appends a newline while parsing.
			d["description"] = OwnedDescription(owner()) + "\n"
			d["net0"] = "bridge=vmbr1,hwaddr=02:00:00:00:00:01,ip=dhcp,name=eth0,type=veth"
			envelope(w, d)
		default:
			envelope(w, task)
		}
	})
	if _, err := c.Start(context.Background(), OwnedCT{VMID: 123, Owner: owner()}); err != nil {
		t.Fatalf("PVE roundtrip rejected: %v", err)
	}
}

func TestPVECannotMaskForeignCurrentConfigWithPendingOwnership(t *testing.T) {
	for _, which := range []string{"foreign current", "foreign pending", "raw lxc"} {
		t.Run(which, func(t *testing.T) {
			var writes atomic.Int32
			c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					writes.Add(1)
					envelope(w, task)
					return
				}
				if r.URL.Path == "/api2/json/pools" {
					envelope(w, poolData())
					return
				}
				o := owner()
				if (which == "foreign current" && r.URL.Query().Get("current") == "1") || (which == "foreign pending" && r.URL.Query().Get("current") != "1") {
					o.ActorUID = "foreign"
				}
				d := configData(o)
				if which == "raw lxc" {
					d["lxc"] = [][]string{{"lxc.mount.entry", "/dev /dev none bind,create=dir 0 0"}}
				}
				envelope(w, d)
			})
			if _, err := c.Start(context.Background(), OwnedCT{VMID: 123, Owner: owner()}); !errors.Is(err, ErrOwnership) || writes.Load() != 0 {
				t.Fatalf("unsafe current/pending config accepted: %v", err)
			}
		})
	}
}

func TestCancellationDuringTaskBodyRead(t *testing.T) {
	c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":`)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := c.WaitTask(ctx, task)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrUncertain) {
		t.Fatalf("lost body cancellation identity: %v", err)
	}
}

func TestPVEResourceMinimum(t *testing.T) {
	var calls atomic.Int32
	c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); envelope(w, task) })
	for _, memory := range []int{1, 15} {
		s := spec()
		s.MemoryMiB = memory
		if _, err := c.Create(context.Background(), s); err == nil {
			t.Fatal("accepted memory below PVE minimum")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid resources reached PVE")
	}
	for _, memory := range []int{16, 1048576} {
		s := spec()
		s.MemoryMiB = memory
		if _, err := c.Create(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEmptyReferencesAndSecretSerialization(t *testing.T) {
	var calls atomic.Int32
	c, cfg := fixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); envelope(w, task) })
	noLeak(t, c, &cfg)
	for _, ref := range []OwnedCT{{}, {VMID: 123}, {VMID: 123, Owner: Ownership{PoolUID: "pool", ActorUID: "actor", Incarnation: 1}}} {
		if _, err := c.Delete(context.Background(), ref); !errors.Is(err, ErrOwnership) {
			t.Fatal("accepted empty reference")
		}
	}
	for _, id := range []int{0, 99, 1000000000} {
		if _, err := c.GetConfig(context.Background(), id); err == nil {
			t.Fatal("accepted invalid VMID")
		}
		if _, err := c.Status(context.Background(), id); err == nil {
			t.Fatal("accepted invalid VMID")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid references reached server")
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"TokenID", "TokenSecret", "CAPEM"} {
		if strings.Contains(string(b), key) {
			t.Fatal("private field serialized")
		}
	}
	// url.Values percent-encoding must preserve the binary assignment separator.
	form, err := c.createForm(spec())
	if err != nil {
		t.Fatal(err)
	}
	encoded := form.Encode()
	decoded, err := url.ParseQuery(encoded)
	if err != nil || decoded.Get("env") != form.Get("env") {
		t.Fatal("form roundtrip failed")
	}
}
