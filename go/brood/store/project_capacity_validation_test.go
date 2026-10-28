package store

import (
	"context"
	"fmt"
	"testing"
)

// FEATURE: Persist only selectors representable by the pinned native API (#69).
// SPEC: Selector.match_labels permits at most ten Kubernetes labels; refusal
// leaves the previously saved complete configuration intact.
func TestProjectCapacityNativeSelectorLimit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.CreateProject(ctx, Project{ID: "p", Name: "project"}); err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{}
	for i := range 10 {
		labels[fmt.Sprintf("label-%d", i)] = "one"
	}
	if err := s.SetProjectCapacity(ctx, "p", ProjectCapacity{WorkerSelector: labels}); err != nil {
		t.Fatalf("native maximum rejected: %v", err)
	}
	labels["eleventh"] = "one"
	if err := s.SetProjectCapacity(ctx, "p", ProjectCapacity{WorkerSelector: labels}); err == nil {
		t.Fatal("accepted eleven labels; native Selector permits at most ten")
	}
	got, err := s.GetProjectCapacity(ctx, "p")
	if err != nil || len(got.WorkerSelector) != 10 {
		t.Fatalf("invalid selector changed stored settings: labels=%d, %v", len(got.WorkerSelector), err)
	}
}
