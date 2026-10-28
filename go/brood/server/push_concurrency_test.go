package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// BUG: глобальная блокировка upload state удерживалась во время registry I/O.
// SPEC: медленный старт одной загрузки не блокирует chunks другой загрузки.
func TestPushSlowStartDoesNotBlockExistingUpload(t *testing.T) {
	blocked, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var starts atomic.Int32
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && starts.Add(1) == 2 {
			close(blocked)
			<-release
		}
		w.Header().Set("Location", "/v2/repo/env/blobs/uploads/registry-id?_state=state")
		w.WriteHeader(202)
	}))
	defer registry.Close()
	origin, _ := url.Parse(registry.URL)
	server := &Server{push: &pushRegistry{origin: origin, client: &http.Client{Timeout: 3 * time.Second}, uploads: map[string]*pushUpload{}}}
	request := func(method string) *http.Request {
		r := httptest.NewRequest(method, "http://brood/api", strings.NewReader("x"))
		r.AddCookie(&http.Cookie{Name: "session", Value: "same-session"})
		r.Header.Set("Content-Type", "application/octet-stream")
		return r
	}
	first := httptest.NewRecorder()
	server.pushStart(first, request("POST"), "repo/env", "user")
	var upload struct {
		ID string `json:"upload_id"`
	}
	if json.Unmarshal(first.Body.Bytes(), &upload) != nil || upload.ID == "" {
		t.Fatal(first.Body.String())
	}
	slowDone := make(chan struct{})
	go func() {
		defer close(slowDone)
		server.pushStart(httptest.NewRecorder(), request("POST"), "repo/env", "user")
	}()
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("slow registry request did not start")
	}
	result := make(chan int, 1)
	go func() {
		r := request("PATCH")
		r.SetPathValue("upload", upload.ID)
		w := httptest.NewRecorder()
		server.pushContinue(w, r, "repo/env", "user")
		result <- w.Code
	}()
	select {
	case status := <-result:
		if status != 204 {
			t.Errorf("chunk status %d", status)
		}
	case <-time.After(time.Second):
		t.Error("existing upload blocked behind a slow registry POST")
	}
	once.Do(func() { close(release) })
	<-slowDone
}
