package store

import (
	"context"
	"testing"
)

// FEATURE: Project command metadata persists without changing base project identity.
// SPEC: Description creation is transactional and does not need a caller-created fixture column.
func TestCommandProjectDescriptionPersists(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedUser(t, s, "operator", "operator")
	if err := s.SetUserAdmin(ctx, "operator", true); err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateAdminProject(ctx, "operator", "example", "private project")
	if err != nil {
		t.Fatal(err)
	}
	var description string
	if err := s.conn(ctx).QueryRowContext(ctx, `SELECT description FROM command_project_details WHERE project_id=?`, p.ID).Scan(&description); err != nil {
		t.Fatal(err)
	}
	if description != "private project" {
		t.Fatalf("description=%q", description)
	}
}
