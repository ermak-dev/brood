package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// BUG: Startup не находит committed root refusal настоящего inline review.
// HYPOTHESIS: Общий selector перечисляет только observed, теряя durable отказ.
func TestPolicyStartupDiscoversOnlySettledInlineReview(t *testing.T) {
	ctx := context.Background()
	for _, state := range []string{"refused", "unknown", "observed"} {
		t.Run(state, func(t *testing.T) {
			s := inlineReviewStore(t, inlineReviewGraph, "reviewer")
			if result := inlineApprove(t, s, 1); !result.Applied {
				t.Fatal("fixture did not enter actual inline review")
			}
			if state == "observed" {
				inlineObserveReview(t, s, "success", strings.Repeat("cd", 20), []string{})
			} else {
				inlineObserveActor(t, s, "success", "", nil, strings.Repeat("cd", 20), "startup-review", state)
			}
			before := databaseRows(t, s.db)
			got, err := s.PendingObservedPipelineSwarms(ctx)
			mustNil(t, err)
			var want []string
			if state != "unknown" {
				want = []string{"one"}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("startup swarms for %s = %v, want %v", state, got, want)
			}
			if _, err := s.PendingObservedPipelineIntent(ctx, "one"); state != "observed" && !errors.Is(err, ErrNotFound) {
				t.Fatalf("refusal/unknown became a verified result: %v", err)
			} else if state == "observed" {
				mustNil(t, err)
			}
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("startup discovery changed durable state")
			}
			stopOne(t, s)
			got, err = s.PendingObservedPipelineSwarms(ctx)
			mustNil(t, err)
			if len(got) != 0 {
				t.Fatal("startup resumed stopped review")
			}
		})
	}
}

func TestPolicyStartupDoesNotDiscoverOrdinaryRefusedObservation(t *testing.T) {
	ctx := context.Background()
	s, observation, _ := policyPreviewResult(t)
	mustNil(t, s.SettlePipelineResultObservation(ctx, observation, true, "known ordinary root refusal"))
	got, err := s.PendingObservedPipelineSwarms(ctx)
	mustNil(t, err)
	if len(got) != 0 {
		t.Fatal("ordinary refusal became inline review bookkeeping")
	}
}
