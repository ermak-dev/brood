package server

// BUG: The owner of a worker under a foreign dispatch lease got refusals that
// named neither the lease holder nor its request ID, so it could not tell whom
// to ask or what to release (#272).
// SPEC: Lifecycle and session control refusals caused by the lease name the
// holder within the swarm and the request ID with its release command, keep
// the conflict class, carry no human identity or task hash, and leave the lease
// in place.
// IMPLEMENTATION: store.DescribeCoordinationHold describes the claim in the
// guard and in authorizeSessionCoordination; lifecycle commands keep the
// coordination reason instead of a generic refusal.
// RESULT: IMPLEMENTED
// STATUS: DONE

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/command"
)

func TestForeignLeaseRefusalNamesHolder(t *testing.T) {
	ts, _, _, other, theirs := foreignLeaseSetup(t)
	ctx := context.Background()
	sessions := recordSessions(ts)
	claim, err := dispatchLease(t, ts, theirs)
	if err != nil {
		t.Fatal(err)
	}
	want := `dispatch lease "before-267" on "theirs" is held by "master"; the holder or an administrator releases it with dispatch --action release --request-id "before-267"`
	refusals := map[string]string{"stop": "", "delete": "", "reset": "", "restart": "release active coordination before restarting or cleaning this actor", "wipe": "release active coordination before restarting or cleaning this actor", "recycle": "release active coordination before restarting or cleaning this actor", "slash": "session control belongs to another dispatch owner", "session_terminal": "session control belongs to another dispatch owner"}
	for tool, reason := range refusals {
		args, lifecycle := lifecycleTargetArguments[tool]
		if !lifecycle {
			args = sessionTargetArguments[tool]
		}
		args = fmt.Sprintf(args, "theirs")
		if got := guestCommand(ts, other, tool, args); got.OK || !strings.Contains(got.Text, want) || !strings.Contains(got.Text, reason) {
			t.Errorf("owner %s under a foreign lease: %+v\nwant a refusal saying %s", tool, got, want)
		}
		var direct error
		if lifecycle {
			_, direct = ts.srv.managed.executeLifecycleCommand(ctx, other, lifecycleInvocation(tool, args))
		} else {
			_, direct = ts.srv.managed.executeGuestSessionCommand(ctx, other, command.Invocation{Name: tool, Arguments: json.RawMessage(args)})
		}
		if !errors.Is(direct, app.ErrConflict) {
			t.Errorf("owner %s refusal lost the conflict class: %v", tool, direct)
		}
		for _, private := range []string{claim.HumanID, claim.PayloadHash} {
			if direct != nil && strings.Contains(direct.Error(), private) {
				t.Errorf("owner %s refusal carries %s: %v", tool, private, direct)
			}
		}
	}
	if got := sessions(); len(got) != 0 {
		t.Fatalf("refused session control reached the guest: %v", got)
	}
	if held, err := dispatchLease(t, ts, theirs); err != nil || held != claim {
		t.Fatalf("refusals changed the lease: %+v %v", held, err)
	}
}
