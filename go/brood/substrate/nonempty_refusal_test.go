package substrate

import (
	"context"
	"errors"
	"testing"

	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
)

// BUG: HTTP409 erases whether a namespace delete was atomically refused (#70).
// SPEC: Only this RPC's FailedPrecondition means nonempty; text, UID/version
// conflicts and uncertain transport outcomes must not trigger a lifecycle rollback.
func TestDeleteAtespaceClassifiesNonemptyWithoutParsingText(t *testing.T) {
	for _, tc := range []struct {
		name     string
		code     codes.Code
		message  string
		status   int
		nonempty bool
	}{
		{"atomic refusal", codes.FailedPrecondition, "arbitrary upstream wording", 409, true},
		{"UID conflict with misleading text", codes.Aborted, "Atespace space is not empty", 409, false},
		{"unavailable", codes.Unavailable, "Atespace space is not empty", 503, false},
		{"deadline", codes.DeadlineExceeded, "request timed out", 502, false},
		{"permission", codes.PermissionDenied, "not permitted", 403, false},
		{"missing", codes.NotFound, "not found", 404, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFake()
			fake.atespaces["space"] = &ateapi.Atespace{Metadata: meta("space", "")}
			fake.failures["DeleteAtespace"] = rpcFailure{code: tc.code, msg: tc.message}
			client, _ := startFake(t, fake)
			err := client.DeleteAtespace(context.Background(), "space", "uid-space")
			if errors.Is(err, ErrAtespaceNotEmpty) != tc.nonempty {
				t.Fatalf("wrong nonempty classification: %v", err)
			}
			var api *APIError
			if !errors.As(err, &api) || api.Status != tc.status || api.Detail != tc.message {
				t.Fatalf("HTTP contract was lost: %v", err)
			}
		})
	}
}
