package clientapi

// FEATURE: Keep credentials on secure origins and secrets out of diagnostics.
// SPEC: Удалённый HTTP и credentials в URL запрещены; отказ logout сохраняет сессию.
// IMPLEMENTATION: Проверяем origin, скрываем недоверенные детали ошибок, сохраняем cookie.
// RESULT: IMPLEMENTED — ошибки сервера, транспорта и redirect не раскрывают секреты.
// STATUS: DONE

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOriginSecurity(t *testing.T) {
	for _, origin := range []string{"http://example.com", "http://192.0.2.1:8000", "https://user:secret@example.com", "https://example.com#secret"} {
		if _, err := New(origin); err == nil {
			t.Errorf("accepted unsafe origin %q", origin)
		} else if strings.Contains(err.Error(), "secret") {
			t.Errorf("secret in error: %v", err)
		}
	}
	for _, origin := range []string{"http://localhost:8000", "http://127.0.0.1:8000", "http://[::1]:8000", "https://example.com"} {
		if _, err := New(origin); err != nil {
			t.Errorf("rejected %q: %v", origin, err)
		}
	}
}

func TestServerErrorsNeverEchoSecrets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "", MaxAge: -1})
		w.WriteHeader(503)
		fmt.Fprint(w, `{"detail":"password-secret session-secret csrf-secret"}`)
	}))
	defer server.Close()
	client, err := New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	saved := StoredSession{Session: "session-secret", CSRF: "csrf-secret"}
	client.Restore(saved)
	err = client.Logout(context.Background())
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe error: %v", err)
	}
	if client.Stored() != saved {
		t.Fatalf("failed logout modified credentials: %+v", client.Stored())
	}
}

func TestRedirectErrorsNeverEchoSecrets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://session-secret.example/secret")
		w.WriteHeader(302)
	}))
	defer server.Close()
	client, err := New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Session(context.Background())
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe redirect diagnostic: %v", err)
	}
}

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("remote diagnostic contains session-secret")
}

func TestTransportErrorsNeverEchoSecrets(t *testing.T) {
	client, err := New("https://example.test")
	if err != nil {
		t.Fatal(err)
	}
	client.http.Transport = failingTransport{}
	_, err = client.Session(context.Background())
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe transport diagnostic: %v", err)
	}
}

func TestMalformedResponsesNeverEchoSecrets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"authenticated":987654321}`)
	}))
	defer server.Close()
	client, err := New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Session(context.Background())
	if err == nil || strings.Contains(err.Error(), "987654321") {
		t.Fatalf("unsafe parse diagnostic: %v", err)
	}
}

func TestSessionStoreRejectsRelativeDirectory(t *testing.T) {
	if _, err := NewSessionStore("relative-secrets"); err == nil {
		t.Fatal("relative secret-state directory accepted")
	}
}
