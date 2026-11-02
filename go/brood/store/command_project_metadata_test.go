package store

// FEATURE: Project creation stores optional operator descriptions atomically.
// SPEC: The real command schema persists descriptions and duplicate creation cannot overwrite them.

import (
	"context"
	"testing"
)

func TestCommandProjectDescription(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedUser(t, s, "admin", "admin")
	if err := s.SetUserAdmin(ctx, "admin", true); err != nil {
		t.Fatal(err)
	}
	project, err := s.CreateAdminProject(ctx, "admin", "documented", "Inventory only, not a source build")
	if err != nil {
		t.Fatal(err)
	}
	var description string
	query := `SELECT description FROM command_project_details WHERE project_id=?`
	if err := s.conn(ctx).QueryRowContext(ctx, query, project.ID).Scan(&description); err != nil {
		t.Fatal(err)
	}
	if description != "Inventory only, not a source build" {
		t.Fatalf("description not persisted: %q", description)
	}
	if _, err := s.CreateAdminProject(ctx, "admin", "documented", "replacement"); err == nil {
		t.Fatal("duplicate name accepted")
	}
	if err := s.conn(ctx).QueryRowContext(ctx, query, project.ID).Scan(&description); err != nil {
		t.Fatal(err)
	}
	if description != "Inventory only, not a source build" {
		t.Fatal("duplicate changed existing description")
	}
}
