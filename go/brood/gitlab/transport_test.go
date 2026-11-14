package gitlab

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestReviewUntrustedTLSNeverSendsCredential(t *testing.T) {
	var hits atomic.Int32
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); fmt.Fprint(w, `{"id":42}`) }))
	defer s.Close()
	c, err := NewClient(sentinel, Config{APIBase: s.URL + "/api/v4", ProjectID: 42})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = c.Project(context.Background())
	checkError(t, err, Transport)
	if hits.Load() != 0 {
		t.Fatal("request reached untrusted TLS server")
	}
}

func TestReviewSameOriginRedirectRefused(t *testing.T) {
	var hits atomic.Int32
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Location", "/api/v4/projects/43")
		w.WriteHeader(http.StatusFound)
	})
	p, err := c.Project(context.Background())
	checkError(t, err, HTTP)
	if hits.Load() != 1 || p != (Project{}) {
		t.Fatal("same-origin redirect followed or partial object returned")
	}
}

func TestReviewOversizedHeadersRefused(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Large", strings.Repeat("x", 40<<10))
		fmt.Fprint(w, `{"id":42}`)
	})
	p, err := c.Project(context.Background())
	checkError(t, err, Transport)
	if p != (Project{}) {
		t.Fatal("partial object returned")
	}
}
