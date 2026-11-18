package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"testing"

	"brood.dev/go/brood/proxmox/attest"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

// FEATURE: Корневая аттестация эфемерного исполнителя до раскрытия payload (#292).
// SPEC: Одна durable-попытка, отдельный registry, точные UID/inc/source,
// проверенный proof до регистрации ребёнка; неизвестный эффект не повторяется.
// IMPLEMENTATION: Внутренний adapter, scoped HMAC proof и durable Store ledger.
// RESULT: IMPLEMENTED
// STATUS: DONE
func ephemeralAttestationServerFixture(t *testing.T) (*dispatchServer, store.EphemeralChildIntent, store.EphemeralChildAllocation) {
	t.Helper()
	d, intent, reservation := ephemeralCapacityFixture(t)
	ctx := context.Background()
	allocation, err := d.srv.allocateEphemeralChild(ctx, d.master.DelegatorID, intent.Parent, intent.ID, reservation)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", d.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`UPDATE managed_actors SET phase='running',stage='done',operation='start' WHERE uid=?`, allocation.ActorUID)
	raw.Close()
	if err != nil {
		t.Fatal(err)
	}
	d.onConnected(func(base commandSessionHub, uid string, incarnation int64) bool {
		return uid == allocation.ActorUID && incarnation == allocation.ActorIncarnation || base.Connected(uid, incarnation)
	})
	return d, intent, allocation
}

func TestEphemeralAttestationRPCCommitsBeforeCallAndRetainsProofWithoutChild(t *testing.T) {
	d, intent, allocation := ephemeralAttestationServerFixture(t)
	ctx := context.Background()
	beforeStep := d.last()
	beforeAttests, beforeMessages := d.calls()
	step := strconv.FormatInt(intent.QueuePosition, 10)
	graphClaim, err := d.srv.steps.attestations.Issue(attest.Request{ActorUID: d.worker.UID, Generation: d.worker.Incarnation, Swarm: d.swarm, Step: step, Origin: intent.Origin, Commit: intent.SourceCommit})
	if err != nil {
		t.Fatal(err)
	}
	key := workspaceAttestationKey(d.srv.managed.registry.relaySecret)
	d.reply = func(call guestAttest) (relay.SessionResponse, error) {
		if call.UID != allocation.ActorUID || call.Generation != allocation.ActorIncarnation || call.Args["swarm"] != d.swarm || call.Args["step"] != step || call.Args["origin"] != intent.Origin || call.Args["commit"] != intent.SourceCommit || call.Args["challenge"] == "" {
			t.Fatalf("foreign executor or source: %+v", call)
		}
		raw, err := sql.Open("sqlite", d.dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer raw.Close()
		var state string
		if err := raw.QueryRow(`SELECT state FROM ephemeral_child_attestations WHERE intent_id=? AND attempt_id='rpc-1'`, intent.ID).Scan(&state); err != nil || state != "pending" {
			t.Fatalf("RPC preceded committed attempt: %q %v", state, err)
		}
		return signedAttestation(t, key, call, call.Generation)
	}
	got, err := d.srv.attestEphemeralChild(ctx, d.master.DelegatorID, intent.Parent, intent.ID, "rpc-1")
	if err != nil || got.State != "attested" || !got.Replay || got.AttestedCommit != intent.SourceCommit || got.AttestedOrigin != intent.Origin {
		t.Fatalf("verified proof missing: %+v %v", got, err)
	}
	if _, err := d.srv.store.CurrentActorOperation(ctx, allocation.ActorUID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("lease not released: %v", err)
	}
	signature, err := attest.Sign(key(graphClaim.ActorUID, graphClaim.Generation), graphClaim)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.srv.steps.attestations.Consume(attest.Attestation{Claim: graphClaim, Signature: signature}, key); err != nil {
		t.Fatalf("child displaced graph challenge: %v", err)
	}
	repeated, err := d.srv.attestEphemeralChild(ctx, d.master.DelegatorID, intent.Parent, intent.ID, "rpc-1")
	if err != nil || repeated != got {
		t.Fatalf("historical retry: %+v %v", repeated, err)
	}
	if _, err := d.srv.attestEphemeralChild(ctx, d.master.DelegatorID, intent.Parent, intent.ID, "rpc-2"); err == nil {
		t.Fatal("new attempt rearmed settled intent")
	}
	afterAttests, afterMessages := d.calls()
	if len(afterAttests) != len(beforeAttests)+1 || len(afterMessages) != len(beforeMessages) || !reflect.DeepEqual(beforeStep, d.last()) {
		t.Fatal("proof repeated RPC, delivered work, or rewrote parent")
	}
	join, err := d.srv.store.GetSwarmChildJoin(ctx, intent.Parent, d.srv.managed.childAuthority)
	if err != nil || len(join.Children) != 0 {
		t.Fatalf("proof exposed child: %+v %v", join, err)
	}
}

func TestEphemeralAttestationRPCUnknownOrInvalidProofNeverReplays(t *testing.T) {
	for _, scenario := range []string{"transport", "unknown", "refused", "garbage", "signature", "challenge", "commit", "incarnation"} {
		t.Run(scenario, func(t *testing.T) {
			d, intent, allocation := ephemeralAttestationServerFixture(t)
			ctx := context.Background()
			before, messages := d.calls()
			key := workspaceAttestationKey(d.srv.managed.registry.relaySecret)
			d.reply = func(call guestAttest) (relay.SessionResponse, error) {
				switch scenario {
				case "transport":
					return relay.SessionResponse{}, errors.New("synthetic disconnected transport")
				case "unknown":
					return relay.SessionResponse{Outcome: "unknown"}, nil
				case "refused":
					return relay.SessionResponse{OK: false, Outcome: "refused"}, nil
				case "garbage":
					return relay.SessionResponse{OK: true, Text: "not JSON"}, nil
				case "challenge":
					call.Args["challenge"] = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
				case "commit":
					call.Args["commit"] = "0000000000000000000000000000000000000000"
				case "incarnation":
					call.Generation++
				}
				response, err := signedAttestation(t, key, call, call.Generation)
				if scenario == "signature" {
					var proof attest.Attestation
					if err := json.Unmarshal([]byte(response.Text), &proof); err != nil {
						t.Fatal(err)
					}
					proof.Signature = "invalid"
					encoded, _ := json.Marshal(proof)
					response.Text = string(encoded)
				}
				return response, err
			}
			if _, err := d.srv.attestEphemeralChild(ctx, d.master.DelegatorID, intent.Parent, intent.ID, "rpc-1"); err == nil {
				t.Fatal("invalid or uncertain proof accepted")
			}
			got, err := d.srv.store.GetEphemeralChildAttestation(ctx, intent.ID)
			if err != nil || got.State != "unconfirmed" || got.AttestedCommit != "" {
				t.Fatalf("uncertainty lost: %+v %v", got, err)
			}
			lease, err := d.srv.store.CurrentActorOperation(ctx, allocation.ActorUID)
			if err != nil || lease.State != "unconfirmed" {
				t.Fatalf("unknown effect lost hold: %+v %v", lease, err)
			}
			repeated, err := d.srv.attestEphemeralChild(ctx, d.master.DelegatorID, intent.Parent, intent.ID, "rpc-1")
			if err != nil || !repeated.Replay || repeated.State != "unconfirmed" {
				t.Fatalf("unknown history retry: %+v %v", repeated, err)
			}
			after, sent := d.calls()
			if len(after) != len(before)+1 || len(sent) != len(messages) {
				t.Fatal("unknown attempt was repeated or delivered payload")
			}
		})
	}
}
