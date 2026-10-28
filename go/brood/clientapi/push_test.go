package clientapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// FEATURE: бинарные chunks используют ту же сессию, CSRF и TLS, что JSON API.
// SPEC: 201/204 поддерживаются; redirect и недоверенные ошибки не раскрывают cookie.
func TestPushStreamSessionAndCreated(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("session")
		if err != nil || cookie.Value != "private-session" || r.Header.Get("X-CSRFToken") != "private-csrf" || r.Header.Get("Origin") == "" {
			t.Error("missing session/CSRF")
		}
		data, _ := io.ReadAll(r.Body)
		if string(data) != "chunk" || r.ContentLength != 5 || r.Header.Get("Content-Type") != "application/octet-stream" {
			t.Errorf("body %q length=%d", data, r.ContentLength)
		}
		w.WriteHeader(201)
		io.WriteString(w, `{"upload_id":"opaque"}`)
	}))
	defer server.Close()
	client, err := New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client.Restore(StoredSession{Session: "private-session", CSRF: "private-csrf"})
	var result struct {
		ID string `json:"upload_id"`
	}
	status, err := client.stream(context.Background(), "POST", "/api/test", strings.NewReader("chunk"), 5, "application/octet-stream", &result)
	if err != nil || status != 201 || result.ID != "opaque" {
		t.Fatalf("%d %+v %v", status, result, err)
	}
}

func TestPushStreamRejectsRedirectAndHidesResponse(t *testing.T) {
	for _, status := range []int{302, 401, 500} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "https://foreign.example/private-session")
			w.WriteHeader(status)
			io.WriteString(w, "private-session private-csrf")
		}))
		client, err := New(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		client.Restore(StoredSession{Session: "private-session", CSRF: "private-csrf"})
		_, err = client.stream(context.Background(), "PATCH", "/api/test", strings.NewReader("chunk"), 5, "application/octet-stream", nil)
		server.Close()
		if err == nil || strings.Contains(err.Error(), "private-session") || strings.Contains(err.Error(), "private-csrf") {
			t.Fatalf("status %d: %v", status, err)
		}
	}
}
