package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestManagedLLMTokenCountingForClaudeCLI(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages/count_tokens" || r.Method != "POST" {
			t.Errorf("wrong route %s %s", r.Method, r.URL.Path)
		}
		io.WriteString(w, `{"input_tokens":12}`)
	}))
	defer backend.Close()
	ts, _, _, base, token := managedLLMSetup(t, backend.URL, "synthetic-key")
	response := llmRequest(t, ts.srv.Handler(), "POST", base+"/v1/messages/count_tokens", token, strings.NewReader(`{"model":"synthetic","messages":[]}`))
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"input_tokens":12`) {
		t.Fatalf("count_tokens: %d %s", response.Code, response.Body)
	}
}

func TestManagedLLMStopCancelsExistingStream(t *testing.T) {
	canceled := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	}))
	defer backend.Close()
	ts, alice, actor, base, token := managedLLMSetup(t, backend.URL, "synthetic-key")
	edge := httptest.NewServer(ts.srv.Handler())
	defer edge.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", edge.URL+base+"/v1/messages", strings.NewReader(`{"stream":true}`))
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	r, body, _ := ts.do(alice, "POST", actor, ts.csrf(alice), map[string]string{"action": "stop"})
	expectStatus(t, "stop active model request", r, body, 200)
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("stopped guest retained its active upstream model stream")
	}
}
