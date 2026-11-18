//go:build linux

package guest

// FEATURE: Root подписывает result только после тихого read-only наблюдения (#290).
// SPEC: UID/incarnation из bootstrap; input/output/pass/sequence связаны подписью;
// observer внутренний, без checkout/step/free; отказ не несёт signed evidence.

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/attest"
	"brood.dev/go/brood/proxmox/guest/sessionctl"
	"brood.dev/go/brood/proxmox/relay"
)

func resultArgs(t *testing.T) json.RawMessage {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal(attestArgs(nil), &fields); err != nil {
		t.Fatal(err)
	}
	fields["pass"] = 1
	fields["sequence"] = 2
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestWorkspaceResultSignsOnlyObservedFactsAfterQuiescence(t *testing.T) {
	rec := &attestRecorder{}
	a := rec.attestor()
	a.Observe = func(_ context.Context, request sessionctl.CheckoutRequest) (sessionctl.ResultFacts, error) {
		rec.events = append(rec.events, "observe")
		if request.Commit != attestCommit || request.Origin != attestOrigin || request.Mode != "gateway" {
			t.Fatal("observer received unbound source")
		}
		return sessionctl.ResultFacts{Base: request.Commit, Head: strings.Repeat("cd", 20), Paths: []string{"go/store/a.go"}}, nil
	}
	handler := newSessionHandler(context.Background(), nil, a, func(context.Context, relay.SessionRequest) relay.SessionResponse {
		t.Fatal("result bypassed root")
		return relay.SessionResponse{}
	})
	response := handler(context.Background(), relay.SessionRequest{Operation: relay.WorkspaceResultOperation, Arguments: resultArgs(t), Caller: "server"})
	if !response.OK || response.Outcome != "complete" {
		t.Fatalf("result: %+v", response)
	}
	if !reflect.DeepEqual(rec.events, []string{"quiesce", "observe", "relaunch", "ensure"}) {
		t.Fatalf("unexpected effects: %v", rec.events)
	}
	signed, err := attest.DecodeResultAttestation([]byte(response.Text))
	if err != nil {
		t.Fatal(err)
	}
	want := attest.ResultRequest{Claim: attest.Claim{ActorUID: "actor-1", Generation: 7, Swarm: "01JSWARM0000000000000000AA", Step: "request-1", Origin: attestOrigin, Commit: attestCommit, Challenge: attestChallenge}, Pass: 1, Sequence: 2}
	if err := attest.VerifyResult(attest.ResultKey(attestToken), want, signed); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(response.Text, attestToken) {
		t.Fatal("root relay token leaked")
	}
}

func TestWorkspaceResultRefusesUnprovenOrIncompleteObservations(t *testing.T) {
	for _, kind := range []string{"unproven-stop", "dirty", "missing-paths", "other-base", "missing-session", "missing-token"} {
		t.Run(kind, func(t *testing.T) {
			rec := &attestRecorder{fail: map[string]error{}}
			if kind == "unproven-stop" {
				rec.fail["quiesce"] = errors.New("unproven stop")
			}
			if kind == "missing-session" {
				rec.fail["ensure"] = errors.New("session unavailable")
			}
			a := rec.attestor()
			if kind == "missing-token" {
				a.Token = ""
			}
			a.Observe = func(context.Context, sessionctl.CheckoutRequest) (sessionctl.ResultFacts, error) {
				facts := sessionctl.ResultFacts{Base: attestCommit, Head: strings.Repeat("cd", 20), Paths: []string{}}
				switch kind {
				case "dirty":
					return sessionctl.ResultFacts{}, errors.New("dirty workspace")
				case "missing-paths":
					facts.Paths = nil
				case "other-base":
					facts.Base = strings.Repeat("ef", 20)
				}
				return facts, nil
			}
			response := a.ObserveResult(context.Background(), relay.SessionRequest{Operation: relay.WorkspaceResultOperation, Arguments: resultArgs(t), Caller: "server"})
			if response.OK || strings.Contains(response.Text, `"signature"`) {
				t.Fatalf("incomplete observation signed: %+v", response)
			}
		})
	}
}

func TestWorkspaceResultRejectsInjectedIdentityAndKeepsHelperInternal(t *testing.T) {
	rec := &attestRecorder{}
	a := rec.attestor()
	a.Observe = func(context.Context, sessionctl.CheckoutRequest) (sessionctl.ResultFacts, error) {
		t.Fatal("invalid request reached observer")
		return sessionctl.ResultFacts{}, nil
	}
	raw := string(resultArgs(t))
	for _, data := range []string{
		strings.Replace(raw, `"pass":1`, `"pass":1,"pass":2`, 1),
		strings.Replace(raw, `"sequence":2`, `"Sequence":2`, 1),
		strings.Replace(raw, `"pass":1`, `"actor_uid":"injected","pass":1`, 1),
		strings.Replace(raw, `"pass":1`, `"pass":null`, 1),
	} {
		response := a.ObserveResult(context.Background(), relay.SessionRequest{Operation: relay.WorkspaceResultOperation, Arguments: json.RawMessage(data), Caller: "server"})
		if response.OK || len(rec.events) != 0 {
			t.Fatalf("invalid request caused effects: %+v", response)
		}
	}
	run := func(context.Context, relay.SessionRequest) relay.SessionResponse {
		t.Fatal("internal result operation escaped")
		return relay.SessionResponse{}
	}
	handler := newSessionHandler(context.Background(), nil, a, run)
	if r := handler(context.Background(), relay.SessionRequest{Operation: sessionctl.ResultOperation, Arguments: json.RawMessage(`{}`), Caller: "server"}); r.OK {
		t.Fatal("relay reached internal observer")
	}
	if r := runSessionHelper(context.Background(), relay.SessionRequest{Operation: sessionctl.ResultOperation, Arguments: json.RawMessage(`{}`), Caller: "server"}); r.OK || !strings.Contains(r.Text, "internal") {
		t.Fatal("stateless helper reached observer")
	}
	unsupported := newSessionHandler(context.Background(), nil, nil, run)
	if r := unsupported(context.Background(), relay.SessionRequest{Operation: relay.WorkspaceResultOperation, Arguments: resultArgs(t), Caller: "server"}); r.OK || r.Outcome != "unsupported" {
		t.Fatal("guest without result attestor pretended support")
	}
}
