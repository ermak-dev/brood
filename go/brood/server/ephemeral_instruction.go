package server

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"brood.dev/go/brood/store"
)

type ephemeralInstructionSource struct {
	Intent     store.EphemeralChildIntent
	Allocation store.EphemeralChildAllocation
	Proof      store.EphemeralChildAttestation
	Actor      store.ManagedActor
	Work       store.SwarmChildWork
}

// Это только текст из согласованных снимков, не проверка текущих прав,
// разрешение отправки, криптографическая проверка или разрешение cleanup.
func ephemeralChildInstruction(in ephemeralInstructionSource) (string, error) {
	intent, allocation, proof, actor, work := in.Intent, in.Allocation, in.Proof, in.Actor, in.Work
	parent, task := intent.Parent, work.Task
	executor := parent.Principal
	executor.ActorUID, executor.Incarnation = allocation.ActorUID, allocation.ActorIncarnation
	if intent.ID == "" || task.ID == "" || intent.RequestID == "" || parent.RequestID == "" || parent.Pass < 1 || parent.Sequence < parent.Pass || parent.Principal.ProjectID == "" || parent.Principal.SwarmID == "" || parent.Principal.AtespaceUID == "" || parent.Principal.ActorUID == "" || parent.Principal.Incarnation < 1 || work.Parent != parent || task.Executor != executor || task.RequestID != intent.RequestID || task.Payload != intent.Payload {
		return "", errors.New("ephemeral child instruction scope is inconsistent")
	}
	if allocation.IntentID != intent.ID || allocation.ActorUID == "" || allocation.ActorUID == parent.Principal.ActorUID || allocation.ActorIncarnation != 1 || actor.UID != allocation.ActorUID || actor.Incarnation != allocation.ActorIncarnation || actor.Name == "" || actor.Class != store.ActorClassEphemeral || actor.Role != "worker" || actor.ParentUID != parent.Principal.ActorUID || actor.SwarmID != parent.Principal.SwarmID || actor.AtespaceUID != parent.Principal.AtespaceUID || actor.PoolUID != intent.PoolUID || actor.Origin != intent.Origin || actor.Template != intent.Template || actor.SourceFingerprint != intent.SourceFingerprint {
		return "", errors.New("ephemeral child instruction executor is inconsistent")
	}
	if intent.Origin == "" || intent.SourceCommit == "" || proof.IntentID != intent.ID || proof.ActorUID != allocation.ActorUID || proof.ActorIncarnation != allocation.ActorIncarnation || proof.State != "attested" || proof.Origin != intent.Origin || proof.Commit != intent.SourceCommit || proof.AttestedOrigin != intent.Origin || proof.AttestedCommit != intent.SourceCommit {
		return "", errors.New("ephemeral child instruction requires retained source attestation")
	}
	if task.Role != "" || task.Delivery != "" || task.State != "pending" && task.State != "running" {
		return "", errors.New("ephemeral child instruction requires an unfinished independent child")
	}
	if strings.TrimSpace(intent.Payload) == "" || len(intent.Payload) > store.MaxSwarmGoalBytes || !utf8.ValidString(intent.Payload) {
		return "", errors.New("ephemeral child instruction payload is invalid")
	}
	text := fmt.Sprintf("[brood ephemeral child task %q] Parent step %d, pass %d.\n\nTask:\n%s\n\nThe server retained workspace source attestation for origin %q at commit %q.\n\nThis is an independent child task, not a graph step. Report once with child_complete: child_id %q, request_id %q, outcome success or fail, and a report describing the result and where it is retained outside this worker. Do not complete the parent's pipeline visit or stop/delete this worker. The server handles cleanup after result confirmation. Workspace attestation is not landing approval. This instruction does not replace policy or owner authorization.", task.ID, parent.Sequence, parent.Pass, intent.Payload, intent.Origin, intent.SourceCommit, task.ID, task.RequestID)
	if strings.Contains(text, envelopeClosingTag) || len(text) > maxStepInstructionBytes {
		return "", errors.New("ephemeral child instruction cannot fit the message envelope")
	}
	if err := deliverable(store.ActorStepAttempt{SwarmID: parent.Principal.SwarmID, Actor: actor}, text); err != nil {
		return "", err
	}
	return text, nil
}
