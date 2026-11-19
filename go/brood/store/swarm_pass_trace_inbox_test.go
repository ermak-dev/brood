package store

// FEATURE: A pass export retains supervisor inbox milestones independently of native delivery.
// SPEC: Selection, custody and dispatch reservation are distinct evidence, never execution authority.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestPassTraceKeepsSupervisorInboxEvidenceSeparateFromNativeDelivery(t *testing.T) {
	ctx := context.Background()
	s := dispatchStore(t)
	a := reserveOne(t, s, "trace-supervisor-inbox")
	input := instructionDeliveryInput(a)
	receiver := strings.Repeat("b", 64)
	intent, err := s.RecordActorStepInboxAttestation(ctx, a, startOrigin, startCommit, input, receiver, allow)
	mustNil(t, err)
	sum := sha256.Sum256([]byte(input.Payload))
	target := ActorDeliveryInboxTarget{DeliveryID: intent.ID, ActorUID: a.Actor.UID, Incarnation: a.Actor.Incarnation,
		SwarmID: a.SwarmID, SourceKind: "step", SourceID: "1", ReceiverID: receiver, Digest: hex.EncodeToString(sum[:])}
	check := func(name string, custody, reserved bool, state string) {
		t.Helper()
		before := databaseRows(t, s.db)
		trace, err := s.GetSwarmPassTrace(ctx, a.SwarmID, 1)
		mustNil(t, err)
		if len(trace.Steps) != 1 || trace.Steps[0].Instruction == nil {
			t.Fatalf("%s: exact recorded instruction is missing", name)
		}
		step := trace.Steps[0]
		delivery := step.Instruction
		if delivery.Intent != intent || len(delivery.Attempts) != 0 || step.Visit.State != state || step.Visit.Outcome != nil {
			t.Fatalf("%s: trace conflated inbox with native send or execution: %+v", name, step)
		}
		raw, err := json.Marshal(delivery)
		mustNil(t, err)
		var observed struct{ SupervisorInbox *ActorDeliveryInboxState }
		mustNil(t, json.Unmarshal(raw, &observed))
		want := ActorDeliveryInboxState{Target: target, Custody: custody, DispatchReserved: reserved}
		if observed.SupervisorInbox == nil || *observed.SupervisorInbox != want {
			t.Fatalf("%s: trace lost the exact supervisor inbox milestone: %+v", name, observed.SupervisorInbox)
		}
		if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
			t.Fatalf("%s: reading trace changed retained source or delivery evidence", name)
		}
	}
	check("selected only", false, false, "delivering")
	mustNil(t, s.RecordActorDeliveryInboxCustody(ctx, target, ActorDeliveryInboxAdmitted, allow))
	check("custody retained", true, false, "delivering")
	_, err = s.ReserveActorDeliveryInboxDispatch(ctx, target, "trace-launch", func(_ context.Context, got ActorDeliveryInboxTarget, launch string) bool {
		return got == target && launch == "trace-launch"
	})
	mustNil(t, err)
	check("permit reserved without arm reply", true, true, "delivering")
	_, err = s.db.Exec(`UPDATE managed_actors SET incarnation=incarnation+1 WHERE uid=?`, a.Actor.UID)
	mustNil(t, err)
	check("historical incarnation", true, true, "delivering")
}
