// Тесты роёв: уникальность atespace_name, изменения состояния и владельца.
package store

import (
	"context"
	"testing"
)

func seedSwarm(t *testing.T, s *Store, id, projectID, ownerID, name string) {
	t.Helper()
	seedProjectWithMember(t, s, projectID, ownerID)
	if err := s.CreateSwarm(context.Background(), Swarm{
		ID: id, ProjectID: projectID, OwnerID: ownerID,
		Name: name, AtespaceName: "swarm-" + id,
	}); err != nil {
		t.Fatalf("CreateSwarm %s: %v", id, err)
	}
}

// TestSwarmDefaultsAndRead проверяет дефолтное состояние provisioning
// и чтение по ID и по имени atespace.
func TestSwarmDefaultsAndRead(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seedSwarm(t, s, "sw1", "p1", "u1", "alpha")

	got, err := s.GetSwarm(ctx, "sw1")
	if err != nil {
		t.Fatalf("GetSwarm: %v", err)
	}
	if got.State != "provisioning" {
		t.Errorf("State = %q, want provisioning", got.State)
	}
	if got.AtespaceUID != nil {
		t.Errorf("AtespaceUID = %v, want nil", got.AtespaceUID)
	}

	byName, err := s.GetSwarmByAtespace(ctx, "swarm-sw1")
	if err != nil {
		t.Fatalf("GetSwarmByAtespace: %v", err)
	}
	if byName.ID != "sw1" {
		t.Errorf("GetSwarmByAtespace ID = %q, want sw1", byName.ID)
	}
	if _, err := s.GetSwarm(ctx, "nope"); !isNotFound(err) {
		t.Errorf("GetSwarm missing = %v, want ErrNotFound", err)
	}
	if _, err := s.GetSwarmByAtespace(ctx, "nope"); !isNotFound(err) {
		t.Errorf("GetSwarmByAtespace missing = %v, want ErrNotFound", err)
	}
}

// TestSwarmUniqueAtespaceName проверяет уникальность atespace_name.
func TestSwarmUniqueAtespaceName(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seedSwarm(t, s, "sw1", "p1", "u1", "alpha")
	err := s.CreateSwarm(ctx, Swarm{
		ID: "sw2", ProjectID: "p1", OwnerID: "u1",
		Name: "beta", AtespaceName: "swarm-sw1",
	})
	if err == nil {
		t.Fatal("duplicate atespace_name accepted")
	}
	if !IsUniqueViolation(err) {
		t.Errorf("error is not unique violation: %v", err)
	}
}

// TestSwarmForeignKeys проверяет FK на project и owner.
func TestSwarmForeignKeys(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seedProjectWithMember(t, s, "p1", "u1")
	err := s.CreateSwarm(ctx, Swarm{
		ID: "swx", ProjectID: "nope", OwnerID: "u1",
		Name: "x", AtespaceName: "swarm-swx",
	})
	if err == nil || !IsForeignKeyViolation(err) {
		t.Errorf("swarm with missing project accepted: %v", err)
	}
	err = s.CreateSwarm(ctx, Swarm{
		ID: "swx", ProjectID: "p1", OwnerID: "nope",
		Name: "x", AtespaceName: "swarm-swx",
	})
	if err == nil || !IsForeignKeyViolation(err) {
		t.Errorf("swarm with missing owner accepted: %v", err)
	}
}

// TestSwarmLists проверяет выборки по проекту и владельцу.
func TestSwarmLists(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seedUser(t, s, "u1", "alice")
	seedUser(t, s, "u2", "bob")
	seedSwarm(t, s, "sw1", "p1", "u1", "alpha")
	seedSwarm(t, s, "sw2", "p1", "u2", "bravo")
	seedSwarm(t, s, "sw3", "p2", "u1", "charlie")

	byProject, err := s.ListSwarmsByProject(ctx, "p1")
	if err != nil {
		t.Fatalf("ListSwarmsByProject: %v", err)
	}
	if len(byProject) != 2 {
		t.Fatalf("ListSwarmsByProject len = %d, want 2", len(byProject))
	}
	if byProject[0].ID != "sw1" || byProject[1].ID != "sw2" {
		t.Errorf("swarms not ordered by created_at/id: %s, %s", byProject[0].ID, byProject[1].ID)
	}

	byOwner, err := s.ListSwarmsByOwner(ctx, "u1")
	if err != nil {
		t.Fatalf("ListSwarmsByOwner: %v", err)
	}
	if len(byOwner) != 2 {
		t.Fatalf("ListSwarmsByOwner len = %d, want 2", len(byOwner))
	}
}

// TestSwarmSetters проверяет SetSwarmState, SetSwarmAtespaceUID,
// SetSwarmOwner и ErrNotFound на отсутствующем рое.
func TestSwarmSetters(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seedSwarm(t, s, "sw1", "p1", "u1", "alpha")
	seedUser(t, s, "u2", "bob")

	if err := s.SetSwarmState(ctx, "sw1", "active"); err != nil {
		t.Fatalf("SetSwarmState: %v", err)
	}
	uid := "ate-uid-1"
	if err := s.SetSwarmAtespaceUID(ctx, "sw1", &uid); err != nil {
		t.Fatalf("SetSwarmAtespaceUID: %v", err)
	}
	if err := s.SetSwarmOwner(ctx, "sw1", "u2"); err != nil {
		t.Fatalf("SetSwarmOwner: %v", err)
	}

	got, err := s.GetSwarm(ctx, "sw1")
	if err != nil {
		t.Fatalf("GetSwarm: %v", err)
	}
	if got.State != "active" {
		t.Errorf("State = %q, want active", got.State)
	}
	if got.AtespaceUID == nil || *got.AtespaceUID != "ate-uid-1" {
		t.Errorf("AtespaceUID = %v, want pointer to ate-uid-1", got.AtespaceUID)
	}
	if got.OwnerID != "u2" {
		t.Errorf("OwnerID = %q, want u2", got.OwnerID)
	}

	if err := s.SetSwarmState(ctx, "nope", "active"); !isNotFound(err) {
		t.Errorf("SetSwarmState missing = %v, want ErrNotFound", err)
	}
	if err := s.SetSwarmAtespaceUID(ctx, "nope", &uid); !isNotFound(err) {
		t.Errorf("SetSwarmAtespaceUID missing = %v, want ErrNotFound", err)
	}
	if err := s.SetSwarmOwner(ctx, "nope", "u2"); !isNotFound(err) {
		t.Errorf("SetSwarmOwner missing = %v, want ErrNotFound", err)
	}
	if err := s.SetSwarmOwner(ctx, "sw1", "nope"); err == nil || !IsForeignKeyViolation(err) {
		t.Errorf("SetSwarmOwner to missing user = %v, want FK violation", err)
	}
}

// TestSetSwarmAtespaceUIDNull проверяет запись NULL в atespace_uid.
func TestSetSwarmAtespaceUIDNull(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seedSwarm(t, s, "sw1", "p1", "u1", "alpha")
	uid := "ate-uid-1"
	if err := s.SetSwarmAtespaceUID(ctx, "sw1", &uid); err != nil {
		t.Fatalf("SetSwarmAtespaceUID: %v", err)
	}
	if err := s.SetSwarmAtespaceUID(ctx, "sw1", nil); err != nil {
		t.Fatalf("SetSwarmAtespaceUID nil: %v", err)
	}
	got, err := s.GetSwarm(ctx, "sw1")
	if err != nil {
		t.Fatalf("GetSwarm: %v", err)
	}
	if got.AtespaceUID != nil {
		t.Errorf("AtespaceUID = %v, want nil", got.AtespaceUID)
	}
}
