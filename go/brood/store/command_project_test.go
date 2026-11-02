package store

// FEATURE: Atomic project actor-limit patches.
// SPEC: Other capacity columns and concurrent changes survive, including unlimited and zero.

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

func TestCommandProjectActorPatch(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedUser(t, s, "admin", "admin")
	if err := s.SetUserAdmin(ctx, "admin", true); err != nil {
		t.Fatal(err)
	}
	seedProject(t, s, "project", "project")
	n := int64(8)
	if err := s.SetProjectCapacity(ctx, "project", ProjectCapacity{WorkerSelector: map[string]string{"zone": "private"}, MaxSwarms: &n, MaxCPUMillicores: &n, MaxMemoryBytes: &n}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errors := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := int64(0); i < 30; i++ {
			if err := s.SetProjectActorLimit(ctx, "admin", "project", &i); err != nil {
				errors <- err
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := int64(1); i <= 30; i++ {
			if _, err := s.conn(ctx).ExecContext(ctx, `UPDATE project_capacity SET max_memory_bytes=? WHERE project_id='project'`, i); err != nil {
				errors <- err
				return
			}
		}
	}()
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Fatal(err)
	}
	cap, err := s.GetProjectCapacity(ctx, "project")
	if err != nil {
		t.Fatal(err)
	}
	if cap.MaxActors == nil || *cap.MaxActors != 29 || cap.MaxMemoryBytes == nil || *cap.MaxMemoryBytes != 30 || cap.MaxSwarms == nil || *cap.MaxSwarms != 8 || cap.MaxCPUMillicores == nil || *cap.MaxCPUMillicores != 8 || cap.WorkerSelector["zone"] != "private" {
		t.Fatalf("partial patch lost changes: %+v", cap)
	}
	if err := s.SetProjectActorLimit(ctx, "admin", "project", nil); err != nil {
		t.Fatal(err)
	}
	cap, err = s.GetProjectCapacity(ctx, "project")
	if err != nil || cap.MaxActors != nil || *cap.MaxMemoryBytes != 30 {
		t.Fatalf("unlimited patch failed: %+v %v", cap, err)
	}
	for _, user := range []string{"ordinary", "disabled"} {
		seedUser(t, s, user, user)
	}
	if err := s.SetUserDisabled(ctx, "disabled", true); err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	for _, user := range []string{"ordinary", "disabled", "missing"} {
		if err := s.SetProjectActorLimit(ctx, user, "project", &zero); err == nil {
			t.Fatal(fmt.Sprintf("%s patched capacity", user))
		}
	}
	negative := int64(-1)
	if err := s.SetProjectActorLimit(ctx, "admin", "project", &negative); err == nil {
		t.Fatal("negative limit accepted")
	}
	if err := s.SetProjectActorLimit(ctx, "admin", "missing", &zero); err == nil {
		t.Fatal("missing project accepted")
	}
}
