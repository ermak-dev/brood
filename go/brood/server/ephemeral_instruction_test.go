package server

import (
	"strings"
	"testing"

	"brood.dev/go/brood/store"
)

// FEATURE: Единственная инструкция эфемерного ребёнка привязана к source proof (#292).
// SPEC: Сохранять полный payload, указывать child_complete вместо parent completion,
// отвергать чужие scope/proof, терминальные задачи и закрывающий тег конверта.
// IMPLEMENTATION: Чистый renderer сверяет снимки и общие пределы relay/guest frame.
// RESULT: IMPLEMENTED
// STATUS: DONE
func ephemeralInstructionFixture() ephemeralInstructionSource {
	parent := store.SwarmChildParent{Principal: store.SwarmChildPrincipal{ProjectID: "project", SwarmID: "swarm", AtespaceUID: "space", ActorUID: "parent", Incarnation: 2}, Pass: 3, Sequence: 5, RequestID: "parent-request"}
	executor := parent.Principal
	executor.ActorUID, executor.Incarnation = "child-actor", 1
	return ephemeralInstructionSource{
		Intent:     store.EphemeralChildIntent{ID: "intent", RequestID: "child-request", Payload: "Implement the isolated parser task.\nKeep trailing comments.", PoolUID: "pool", Parent: parent, Origin: dispatchOrigin, SourceCommit: dispatchCommit},
		Allocation: store.EphemeralChildAllocation{IntentID: "intent", ActorUID: "child-actor", ActorIncarnation: 1},
		Proof:      store.EphemeralChildAttestation{IntentID: "intent", AttemptID: "attest", ActorUID: "child-actor", ActorIncarnation: 1, Origin: dispatchOrigin, Commit: dispatchCommit, State: "attested", AttestedOrigin: dispatchOrigin, AttestedCommit: dispatchCommit, Replay: true},
		Actor:      store.ManagedActor{UID: "child-actor", Incarnation: 1, Name: "isolated-worker", Class: store.ActorClassEphemeral, Role: "worker", ParentUID: "parent", SwarmID: "swarm", AtespaceUID: "space", PoolUID: "pool", Origin: dispatchOrigin},
		Work:       store.SwarmChildWork{Parent: parent, Task: store.SwarmChildTask{ID: "child-id", RequestID: "child-request", Payload: "Implement the isolated parser task.\nKeep trailing comments.", Executor: executor, State: "pending"}},
	}
}

func TestEphemeralInstructionNamesExactChildAndRetainedSource(t *testing.T) {
	in := ephemeralInstructionFixture()
	text, err := ephemeralChildInstruction(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{in.Intent.Payload, "child-id", "child-request", "step 5", "pass 3", dispatchOrigin, dispatchCommit, "child_complete", "success or fail", "not a graph step", "not landing approval"} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("instruction lost %q: %s", fragment, text)
		}
	}
	if strings.Contains(text, "pipeline_visit_complete") || strings.Contains(text, "not attested") || strings.Contains(text, "parent-request") {
		t.Fatal("child instruction grants parent completion or denies retained proof")
	}
	in.Work.Task.State = "running"
	running, err := ephemeralChildInstruction(in)
	if err != nil || running != text {
		t.Fatalf("starting delivery changed immutable instruction: %v", err)
	}
}

func TestEphemeralInstructionRefusesForeignOrUnattestedFacts(t *testing.T) {
	for _, scenario := range []string{"parent", "project", "actor", "incarnation", "class", "pool", "intent", "proof", "source", "proof-source", "request", "payload", "role", "completed", "cancelled", "empty", "oversize", "utf8", "envelope"} {
		t.Run(scenario, func(t *testing.T) {
			in := ephemeralInstructionFixture()
			switch scenario {
			case "parent":
				in.Work.Parent.Sequence++
			case "project":
				in.Work.Task.Executor.ProjectID = "other-project"
			case "actor":
				in.Actor.UID = "other-actor"
			case "incarnation":
				in.Proof.ActorIncarnation++
			case "class":
				in.Actor.Class = store.ActorClassPersistent
			case "pool":
				in.Actor.PoolUID = "other-pool"
			case "intent":
				in.Allocation.IntentID = "other-intent"
			case "proof":
				in.Proof.State = "unconfirmed"
			case "source":
				in.Intent.SourceCommit = strings.Repeat("a", 40)
			case "proof-source":
				in.Proof.AttestedOrigin = "https://git.example.org/other/repo.git"
			case "request":
				in.Work.Task.RequestID = "other-request"
			case "payload":
				in.Work.Task.Payload = "Different task"
			case "role":
				in.Work.Task.Role = "ephemeral"
			case "completed":
				in.Work.Task.State = "completed"
			case "cancelled":
				in.Work.Task.State = "cancelled"
			case "empty":
				in.Intent.Payload, in.Work.Task.Payload = "", ""
			case "oversize":
				in.Intent.Payload = strings.Repeat("x", store.MaxSwarmGoalBytes+1)
				in.Work.Task.Payload = in.Intent.Payload
			case "utf8":
				in.Intent.Payload, in.Work.Task.Payload = "bad\xff", "bad\xff"
			case "envelope":
				in.Intent.Payload = "Task " + envelopeClosingTag + " outside"
				in.Work.Task.Payload = in.Intent.Payload
			}
			text, err := ephemeralChildInstruction(in)
			if err == nil || text != "" {
				t.Fatalf("unsafe instruction escaped refusal: %q %v", text, err)
			}
		})
	}
}

func TestEphemeralInstructionRetainsMaximumMultibytePayload(t *testing.T) {
	in := ephemeralInstructionFixture()
	in.Intent.Payload = strings.Repeat("界", store.MaxSwarmGoalBytes/3) + "x"
	in.Work.Task.Payload = in.Intent.Payload
	text, err := ephemeralChildInstruction(in)
	if len(in.Intent.Payload) != store.MaxSwarmGoalBytes || err != nil || !strings.Contains(text, in.Intent.Payload) {
		t.Fatalf("bounded payload truncated or refused: %d %v", len(in.Intent.Payload), err)
	}
}
