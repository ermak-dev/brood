package clientapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// FEATURE: Типизированный клиент синхронизации проекта #172.
// SPEC: PUT использует существующий CSRF/session transport и точные JSON поля.
// IMPLEMENTATION: Типизированный результат и поддержка 201 в общем transport.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestProjectSyncClient(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("X-CSRFToken") != "csrf" || r.Header.Get("Origin") == "" {
			t.Error("missing normal CSRF transport")
		}
		if cookie, err := r.Cookie("session"); err != nil || cookie.Value != "session" {
			t.Error("missing normal session transport")
		}
		if r.Method != "PUT" || r.URL.Path != "/api/projects/sync" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["name"] != "demo" || body["repository"] != "" {
			t.Errorf("body: %v %v", body, err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		fmt.Fprint(w, `{"id":"p","name":"demo","repository":"","created":true,"changed":true,"origin_mode":""}`)
	}))
	defer srv.Close()
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	c.Restore(StoredSession{Session: "session", CSRF: "csrf"})
	resultValue, err := c.ProjectSync(context.Background(), map[string]any{"name": "demo", "repository": ""})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(resultValue)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	json.Unmarshal(data, &result)
	if calls != 1 || result["id"] != "p" || result["name"] != "demo" || result["repository"] != "" || result["created"] != true || result["changed"] != true || result["origin_mode"] != "" {
		t.Fatalf("result: %s, calls=%d", data, calls)
	}
}
