// Тесты проектов и членства: CRUD, уникальность имени, составной PK.
package store

import (
	"context"
	"testing"
)

// TestProjectCRUD проверяет создание и чтение проекта.
func TestProjectCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.CreateProject(ctx, Project{ID: "p1", Name: "infra"}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	got, err := s.GetProject(ctx, "p1")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.Name != "infra" {
		t.Errorf("Name = %q, want infra", got.Name)
	}
	if got.CreatedAt == nil {
		t.Error("CreatedAt not set by store")
	}

	if err := s.CreateProject(ctx, Project{ID: "p2", Name: "infra"}); err == nil {
		t.Fatal("duplicate project name accepted")
	} else if !IsUniqueViolation(err) {
		t.Errorf("error is not unique violation: %v", err)
	}

	if _, err := s.GetProject(ctx, "nope"); !isNotFound(err) {
		t.Errorf("GetProject missing = %v, want ErrNotFound", err)
	}
}

// TestProjectDeleteCascadeMembers проверяет каскадное удаление членства
// вместе с проектом.
func TestProjectDeleteCascadeMembers(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seedProjectWithMember(t, s, "p1", "u1")
	if err := s.DeleteProject(ctx, "p1"); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM project_members`).Scan(&n); err != nil {
		t.Fatalf("count project_members: %v", err)
	}
	if n != 0 {
		t.Errorf("project_members rows after project delete = %d, want 0", n)
	}
}

// TestMembership проверяет AddMember, MembersOf, IsMember, RemoveMember
// и отказ при повторном добавлении.
func TestMembership(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seedUser(t, s, "u1", "alice")
	seedUser(t, s, "u2", "bob")
	if err := s.CreateProject(ctx, Project{ID: "p1", Name: "infra"}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	if err := s.AddMember(ctx, "p1", "u1"); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	if err := s.AddMember(ctx, "p1", "u1"); err == nil {
		t.Fatal("duplicate membership accepted")
	} else if !IsUniqueViolation(err) {
		t.Errorf("error is not unique violation: %v", err)
	}
	if err := s.AddMember(ctx, "p1", "nope"); err == nil {
		t.Fatal("membership of missing user accepted")
	} else if !IsForeignKeyViolation(err) {
		t.Errorf("error is not foreign key violation: %v", err)
	}

	members, err := s.MembersOf(ctx, "p1")
	if err != nil {
		t.Fatalf("MembersOf: %v", err)
	}
	if len(members) != 1 || members[0].Username != "alice" {
		t.Fatalf("members = %v, want [alice]", members)
	}

	ok, err := s.IsMember(ctx, "p1", "u1")
	if err != nil {
		t.Fatalf("IsMember: %v", err)
	}
	if !ok {
		t.Error("IsMember(p1, u1) = false, want true")
	}
	ok, err = s.IsMember(ctx, "p1", "u2")
	if err != nil {
		t.Fatalf("IsMember: %v", err)
	}
	if ok {
		t.Error("IsMember(p1, u2) = true, want false")
	}

	if err := s.RemoveMember(ctx, "p1", "u1"); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}
	if err := s.RemoveMember(ctx, "p1", "u1"); !isNotFound(err) {
		t.Errorf("RemoveMember missing = %v, want ErrNotFound", err)
	}
	ok, _ = s.IsMember(ctx, "p1", "u1")
	if ok {
		t.Error("IsMember after RemoveMember = true")
	}
}

// TestRemoveMemberCascadeSwarms: исключение из проекта не удаляет рои,
// но разрывает членство.
func TestRemoveMemberCascadeSwarms(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seedProjectWithMember(t, s, "p1", "u1")
	if err := s.CreateSwarm(ctx, Swarm{
		ID: "sw1", ProjectID: "p1", OwnerID: "u1",
		Name: "alpha", AtespaceName: "swarm-alpha",
	}); err != nil {
		t.Fatalf("CreateSwarm: %v", err)
	}
	if err := s.RemoveMember(ctx, "p1", "u1"); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}
	sw, err := s.GetSwarm(ctx, "sw1")
	if err != nil {
		t.Fatalf("GetSwarm after RemoveMember: %v", err)
	}
	if sw.OwnerID != "u1" {
		t.Errorf("OwnerID = %q, want u1", sw.OwnerID)
	}
}

// TestUserDeleteCascade проверяет каскады при удалении пользователя.
func TestUserDeleteCascade(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seedProjectWithMember(t, s, "p1", "u1")
	if err := s.CreateSession(ctx, Session{TokenHash: "t1", UserID: "u1"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := s.DeleteUser(ctx, "u1"); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	var m, sess int
	_ = s.db.QueryRowContext(ctx, `SELECT count(*) FROM project_members`).Scan(&m)
	_ = s.db.QueryRowContext(ctx, `SELECT count(*) FROM sessions`).Scan(&sess)
	if m != 0 {
		t.Errorf("project_members rows after user delete = %d, want 0", m)
	}
	if sess != 0 {
		t.Errorf("sessions rows after user delete = %d, want 0", sess)
	}
	if err := s.DeleteUser(ctx, "nope"); !isNotFound(err) {
		t.Errorf("DeleteUser missing = %v, want ErrNotFound", err)
	}
}
