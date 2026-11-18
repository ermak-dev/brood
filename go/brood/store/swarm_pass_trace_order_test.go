package store

import (
	"context"
	"reflect"
	"testing"
)

func TestPassTraceOrdersRetainedQuestionInstantsRatherThanRFC3339Text(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		t.Run(map[bool]string{false: "unscoped", true: "scoped"}[scoped], func(t *testing.T) {
			ctx := context.Background()
			s := dispatchStore(t)
			for _, q := range []struct{ id, timestamp string }{
				{"z-zero", "2040-01-01T00:00:00Z"},
				{"a-quarter", "2040-01-01T00:00:00.25Z"},
				{"0-unknown", "unknown"},
			} {
				_, err := s.db.Exec(`INSERT INTO swarm_requests(id,swarm_id,origin,actor_uid,actor_incarnation,kind,question,options,state,created_at) VALUES(?,'one','actor','a',1,'text','Preserve recorded order.','[]','open',?)`, q.id, q.timestamp)
				mustNil(t, err)
				if scoped {
					_, err = s.db.Exec(`INSERT INTO swarm_request_pass_contexts VALUES(?,'one',1,1)`, q.id)
					mustNil(t, err)
				}
			}
			trace, err := s.GetSwarmPassTrace(ctx, "one", 1)
			mustNil(t, err)
			ids := []string{}
			if scoped {
				for _, q := range trace.Requests {
					ids = append(ids, q.Request.ID)
				}
			} else {
				for _, q := range trace.UnscopedActorRequests {
					ids = append(ids, q.ID)
				}
			}
			if !reflect.DeepEqual(ids, []string{"z-zero", "a-quarter", "0-unknown"}) {
				t.Fatalf("trace sorted timestamps lexically or invented a legacy instant: %v", ids)
			}
			found := false
			for _, gap := range trace.Gaps {
				if gap.Kind == "request_time_unavailable" && gap.RequestID == "0-unknown" {
					found = true
				}
			}
			if !found {
				t.Fatal("trace silently ordered an unknown time as if it were verified")
			}
		})
	}
}
