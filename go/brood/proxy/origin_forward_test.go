package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestActorReceivesMatchingBrowserHostAndOrigin(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin, err := url.Parse(r.Header.Get("Origin"))
		if err != nil || origin.Host != r.Host {
			http.Error(w, "origin mismatch", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	base, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, fixedUser("u", "user"), fakeSwarms{"sw": activeSwarm("sw", "space")})
	h.allow("u")
	h.res.set("space", "actor", base)
	response := h.do(t, "GET", "/swarms/sw/actors/actor/", http.Header{"Origin": []string{h.server.URL}}, nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("actor origin check rejected proxy: %d", response.StatusCode)
	}
}
