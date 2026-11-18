package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// BUG: CLI скрывал публичную причину отказа нового прохода после owner stop.
// SPEC: Ошибка сообщает о reconciliation, не печатает успех и не повторяет POST.
func TestSwarmStartShowsChildReconciliationReason(t *testing.T) {
	const reason = "outstanding child obligations require explicit reconciliation before automatic progression"
	starts := 0
	secretServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/projects":
			fmt.Fprint(w, `[{"id":"p1","name":"demo"}]`)
		case "/api/projects/p1/swarms":
			fmt.Fprint(w, `[{"id":"s3","name":"solo","state":"active"}]`)
		case "/api/swarms/s3/start":
			if r.Method != http.MethodPost {
				t.Errorf("unexpected start method: %s", r.Method)
			}
			starts++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"detail": reason})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	code, output, diagnostic := invoke(t, "", "swarm", "start", "solo", "--definition", "release", "--goal", "new automatic pass")
	if code != 1 || output != "" || !strings.Contains(diagnostic, reason) || starts != 1 {
		t.Fatalf("public refusal lost or replayed: code=%d output=%q diagnostic=%q starts=%d", code, output, diagnostic, starts)
	}
}
