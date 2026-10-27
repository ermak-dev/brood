package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestOrdinaryRequestRevokedWhileWaitingForHeaders(t *testing.T) {
	arrived := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-release:
		}
	}))
	defer up.Close()
	defer close(release)
	h := newHarness(t, fixedUser("u1", "alice"), fakeSwarms{"sw1": activeSwarm("sw1", "at1")})
	h.allow("u1")
	base, _ := url.Parse(up.URL)
	h.res.set("at1", "a1", base)
	done := make(chan struct{})
	go func() {
		resp, err := h.server.Client().Get(h.server.URL + "/swarms/sw1/actors/a1")
		if err == nil {
			resp.Body.Close()
		}
		close(done)
	}()
	<-arrived
	h.authz.set("u1", false)
	h.proxy.RecheckNow()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("ordinary upstream request survived revocation")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("client request survived revocation")
	}
}

func TestRouterTargetAndCredentials(t *testing.T) {
	up := newTestUpstream(t)
	h := newHarness(t, fixedUser("u1", "alice"), fakeSwarms{"sw1": activeSwarm("sw1", "at1")})
	h.allow("u1")
	h.res.set("at1", "a1", up.base(t))
	headers := http.Header{
		"Ate-Target-Actor": {"foreign/actor", "another/actor"},
		"Authorization":    {"Bearer brood-secret"},
		"X-Csrftoken":      {"brood-csrf"},
		"Cookie":           {"session=secret", "csrf=secret; other=kept"},
	}
	resp := h.do(t, "GET", "/swarms/sw1/actors/a1/api/echo", headers, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	hdr, _, _ := up.snapshot()
	if got := hdr.Values("Ate-Target-Actor"); len(got) != 1 || got[0] != "at1/a1" {
		t.Fatalf("router target=%v", got)
	}
	for _, name := range []string{"Authorization", "X-CSRFToken"} {
		if hdr.Get(name) != "" {
			t.Fatalf("credential forwarded: %s", name)
		}
	}
	if got := hdr.Get("Cookie"); got != "other=kept" {
		t.Fatalf("cookies=%q", got)
	}
}
