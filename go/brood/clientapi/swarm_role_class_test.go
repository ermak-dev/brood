package clientapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// BUG: CLI разрешает имя ephemeral как кандидата на постоянную роль workflow.
// SPEC: Класс берётся из inventory; legacy без class остаётся persistent,
// будущий класс не даёт роль, malformed class не превращается в legacy.
// SOLUTION: Явный class разбирается до разрешения имени/UID.
// RESULT: FIXED — прежний клиент отправлял bind ephemeral; новый не делает PUT.
// STATUS: FIXED
func TestSwarmRoleCandidatesRespectExecutorClass(t *testing.T) {
	for _, tc := range []struct {
		name, rows string
		want       []SwarmActor
		invalid    bool
	}{
		{name: "classes", rows: `{"metadata":{"uid":"legacy","name":"legacy"},"provider":"proxmox","generation":"2","phase":"running"},{"metadata":{"uid":"persistent","name":"persistent"},"provider":"proxmox","class":"persistent","generation":"3","phase":"running"},{"metadata":{"uid":"ephemeral","name":"ephemeral"},"provider":"proxmox","class":"ephemeral","generation":"1","phase":"running"},{"metadata":{"uid":"future","name":"future"},"provider":"proxmox","class":"future","generation":"1","phase":"running"}`, want: []SwarmActor{{UID: "legacy", Name: "legacy", Incarnation: 2, Phase: "running"}, {UID: "persistent", Name: "persistent", Incarnation: 3, Phase: "running"}}},
		{name: "null", rows: `{"provider":"proxmox","class":null,"metadata":{"uid":"x","name":"x"},"generation":"1"}`, invalid: true},
		{name: "empty", rows: `{"provider":"proxmox","class":"","metadata":{"uid":"x","name":"x"},"generation":"1"}`, invalid: true},
		{name: "array", rows: `{"provider":"proxmox","class":["persistent"],"metadata":{"uid":"x","name":"x"},"generation":"1"}`, invalid: true},
		{name: "native", rows: `{"metadata":{"uid":"native","name":"native"},"class":null}`, want: []SwarmActor{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reads++
				if r.Method != "GET" || r.URL.Path != "/api/swarms/one/actors" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				fmt.Fprintf(w, `{"results":[%s]}`, tc.rows)
			}))
			defer server.Close()
			client, err := New(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			client.Restore(StoredSession{Session: "fixture-session", CSRF: "fixture-csrf"})
			got, err := client.SwarmActors(context.Background(), "one")
			if tc.invalid {
				if err == nil {
					t.Fatalf("malformed class accepted as role candidate: %+v", got)
				}
			} else if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("role candidates = %+v, %v; want %+v", got, err, tc.want)
			}
			if reads != 1 {
				t.Fatalf("role listing retried or caused extra effects: %d", reads)
			}
		})
	}
}
