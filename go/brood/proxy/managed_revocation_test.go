package proxy

// FEATURE: Provider incarnation revocation for live proxy requests.
// SPEC: A resolved managed target is revalidated alongside its original session;
// reset/deletion closes requests already waiting for upstream response headers.

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"brood.dev/go/brood/app"
)

func TestManagedTargetRevocation(t *testing.T) {
	var valid atomic.Bool
	valid.Store(true)
	dialed := make(chan struct{})
	a, b := net.Pipe()
	defer b.Close()
	cfg := Config{Swarms: fakeSwarms{"s": {ID: "s", AtespaceName: "ns", State: app.StateActive}}, Authorize: func(string, app.Action, app.Resource) (bool, error) { return true, nil }, CurrentUser: fixedUser("u", "user"), Upstream: &fakeResolver{bases: map[string]*url.URL{"ns/a": {Scheme: "http", Host: "incarnation.invalid:8080"}}}, DialContext: func(context.Context, string, string) (net.Conn, error) { close(dialed); return a, nil }}
	cfg.UpstreamValid = func(*url.URL) bool { return valid.Load() }
	p := New(cfg)
	defer p.Close()
	mux := http.NewServeMux()
	p.Mount(mux)
	done := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/swarms/s/actors/a/", nil))
		done <- w.Code
	}()
	<-dialed
	valid.Store(false)
	p.RecheckNow()
	select {
	case status := <-done:
		if status != 502 {
			t.Fatalf("revoked response status %d", status)
		}
	case <-time.After(time.Second):
		t.Fatal("managed generation revocation left HTTP request open")
	}
}
