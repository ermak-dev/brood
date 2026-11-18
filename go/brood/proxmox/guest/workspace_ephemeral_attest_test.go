//go:build linux

package guest

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/attest"
	"brood.dev/go/brood/proxmox/guest/sessionctl"
)

// FEATURE: Root-аттестация эфемерного worker ограничена bootstrap commit (#292).
// SPEC: Чужой commit отвергается до quiesce/checkout; подготовка идёт в том же
// тихом окне до checkout; отказ не запускает свободную модель и не подписывается.
// IMPLEMENTATION: Общий budget prepare/checkout и fail-closed ephemeral ветка.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestEphemeralAttestationRejectsCommitOutsideBootstrapPin(t *testing.T) {
	recorder := &attestRecorder{}
	a := recorder.attestor()
	a.Origin.Ephemeral = &EphemeralWorkspace{Commit: strings.Repeat("c", 40)}
	a.Prepare = func(context.Context, sessionctl.CheckoutRequest) error { return nil }
	response := attestCall(a, attestArgs(nil))
	if response.OK || len(recorder.events) != 0 || !strings.Contains(response.Text, "bootstrap commit") {
		t.Fatalf("foreign bootstrap commit reached root effects or signature: events=%v response=%+v", recorder.events, response)
	}
}

func TestEphemeralAttestationPreparesInsideQuietWindowBeforeFirstSession(t *testing.T) {
	for _, scenario := range []string{"success", "prepare", "checkout", "facts", "step", "ensure", "missing-prepare"} {
		t.Run(scenario, func(t *testing.T) {
			recorder := &attestRecorder{fail: map[string]error{}}
			a := recorder.attestor()
			a.Origin.Ephemeral = &EphemeralWorkspace{Commit: attestCommit}
			var deadline time.Time
			a.Prepare = func(ctx context.Context, request sessionctl.CheckoutRequest) error {
				recorder.events = append(recorder.events, "prepare")
				var bounded bool
				deadline, bounded = ctx.Deadline()
				if !bounded || time.Until(deadline) > checkoutHelperTimeout || request.Commit != attestCommit || request.Origin != attestOrigin || request.Mode != "gateway" {
					t.Fatal("preparation lost shared budget or bootstrap source")
				}
				if scenario == "prepare" {
					return errors.New("preparation refused")
				}
				return nil
			}
			checkout := a.Checkout
			a.Checkout = func(ctx context.Context, request sessionctl.CheckoutRequest) (sessionctl.CheckoutFacts, error) {
				if until, ok := ctx.Deadline(); !ok || until != deadline {
					t.Fatal("checkout did not share preparation budget")
				}
				return checkout(ctx, request)
			}
			if scenario == "checkout" || scenario == "step" || scenario == "ensure" {
				recorder.fail[scenario] = errors.New("synthetic refusal")
			}
			if scenario == "facts" {
				recorder.facts = &sessionctl.CheckoutFacts{Commit: strings.Repeat("c", 40)}
			}
			if scenario == "missing-prepare" {
				a.Prepare = nil
			}
			response := attestCall(a, attestArgs(nil))
			want := []string{"quiesce", "prepare", "checkout", "step", "relaunch", "ensure"}
			switch scenario {
			case "prepare":
				want = []string{"quiesce", "prepare", "relaunch"}
			case "checkout", "facts":
				want = []string{"quiesce", "prepare", "checkout", "relaunch"}
			case "step":
				want = []string{"quiesce", "prepare", "checkout", "step", "relaunch"}
			case "missing-prepare":
				want = nil
			}
			if !reflect.DeepEqual(recorder.events, want) {
				t.Fatalf("unsafe ephemeral startup order: got %v, want %v", recorder.events, want)
			}
			if response.OK != (scenario == "success") {
				t.Fatalf("wrong ephemeral proof verdict: %+v", response)
			}
			if scenario == "success" {
				proof, err := attest.DecodeAttestation([]byte(response.Text))
				if err != nil || proof.Commit != attestCommit || proof.ActorUID != a.ActorUID || proof.Generation != a.Generation {
					t.Fatalf("exact bootstrap proof missing: %+v %v", proof, err)
				}
			} else if strings.Contains(response.Text, `"signature"`) {
				t.Fatal("refused bootstrap retained a signature")
			}
			if strings.Contains(response.Text, attestToken) {
				t.Fatal("bootstrap token leaked into response")
			}
		})
	}
}
