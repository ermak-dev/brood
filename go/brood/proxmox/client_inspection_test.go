package proxmox

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// FEATURE: Owned restart inspection and positive cluster-wide VMID absence proof.
// SPEC: Never infer absence from scoped inventory or permission/server failures.
func TestInspectOwnedValidatesAllViewsAndPool(t *testing.T) {
	for _, scenario := range []string{"owned", "foreign-current", "foreign-pending", "foreign-final", "digest-changed", "not-member"} {
		t.Run(scenario, func(t *testing.T) {
			reads := 0
			c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Error("inspection wrote to PVE")
				}
				if r.URL.Path == "/api2/json/pools" {
					if scenario == "not-member" {
						envelope(w, []any{})
					} else {
						envelope(w, poolData())
					}
					return
				}
				reads++
				d := ociConfigData()
				if scenario == "foreign-current" && reads == 1 || scenario == "foreign-pending" && reads == 2 || scenario == "foreign-final" && reads == 3 {
					d["description"] = "foreign"
				}
				if scenario == "digest-changed" && reads == 3 {
					d["digest"] = strings.Repeat("b", 40)
				}
				envelope(w, d)
			})
			got, err := c.InspectOwned(context.Background(), OwnedCT{123, owner()})
			if scenario == "owned" {
				if err != nil || got.Hostname != "actor" {
					t.Fatalf("%+v %v", got, err)
				}
				noLeak(t, got)
			} else if err == nil {
				t.Fatal("unsafe inspection accepted")
			}
		})
	}
}
func TestNotFoundIsOnlyHTTP404(t *testing.T) {
	for _, status := range []int{404, 403, 500, 502} {
		c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) })
		_, err := c.GetConfig(context.Background(), 123)
		if errors.Is(err, ErrNotFound) != (status == 404) {
			t.Fatalf("status %d: %v", status, err)
		}
	}
}
func TestConfirmAbsentRequiresExactGlobalFreeID(t *testing.T) {
	for _, scenario := range []string{"free-string", "free-number", "wrong-id", "malformed", "exists", "permission", "unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/api2/json/cluster/nextid" || r.URL.RawQuery != "vmid=123" {
					t.Error("absence was not checked globally for exact ID")
				}
				switch scenario {
				case "free-string":
					envelope(w, "123")
				case "free-number":
					envelope(w, 123)
				case "wrong-id":
					envelope(w, 124)
				case "malformed":
					envelope(w, "0123")
				case "exists":
					w.WriteHeader(400)
				case "permission":
					w.WriteHeader(403)
				default:
					w.WriteHeader(500)
				}
			})
			err := c.ConfirmAbsent(context.Background(), 123)
			if (err == nil) != (scenario == "free-string" || scenario == "free-number") {
				t.Fatalf("absence %s: %v", scenario, err)
			}
		})
	}
	c, _ := fixture(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid VMID requested") })
	if err := c.ConfirmAbsent(context.Background(), 1); err == nil {
		t.Fatal("invalid VMID accepted")
	}
}
