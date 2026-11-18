package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// Класс актора (#292) не меняет прежний admission: он создаёт persistent.
// Эфемерный инвентарь не становится ролевым актором или глобальным спрашивающим.
func TestManagedActorClassDefaultsPersistentAndRejectsOrdinaryEphemeralAdmission(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	managedFixture(t, s)
	a, err := s.ReserveManagedActor(ctx, managedRequest("persistent", "one", 101))
	mustNil(t, err)
	if a.Class != ActorClassPersistent {
		t.Fatalf("ordinary admission class: %q", a.Class)
	}
	reread, err := s.GetManagedActor(ctx, a.UID)
	mustNil(t, err)
	if reread.Class != ActorClassPersistent {
		t.Fatal("actor class was not retained")
	}
	for _, class := range []string{ActorClassEphemeral, "unknown"} {
		in := managedRequest("forged-"+class, "one", 102)
		in.Class = class
		before := databaseRows(t, s.db)
		if _, err := s.ReserveManagedActor(ctx, in); err == nil {
			t.Fatal("ordinary admission accepted caller-selected ephemeral/unknown class")
		}
		if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
			t.Fatal("refused class consumed capacity or wrote inventory")
		}
	}
}

func classFixture(t *testing.T) (*Store, ManagedActor, ManagedActor) {
	t.Helper()
	ctx := context.Background()
	s := newTestStore(t)
	managedFixture(t, s)
	parent, err := s.ReserveManagedActor(ctx, managedRequest("parent", "one", 101))
	mustNil(t, err)
	in := managedRequest("ephemeral", "one", 102)
	in.Class, in.ParentUID = ActorClassEphemeral, parent.UID
	ephemeral, err := s.reserveManagedActor(ctx, in)
	mustNil(t, err)
	return s, parent, ephemeral
}

func TestManagedEphemeralClassCannotBindRoleOrAskWithoutStepContext(t *testing.T) {
	s, parent, a := classFixture(t)
	ctx := context.Background()
	mustNil(t, s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "one", Role: "review", ActorUID: parent.UID}))
	before := databaseRows(t, s.db)
	err := s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "one", Role: "build", ActorUID: a.UID})
	if !errors.Is(err, ErrSwarmRoleInactive) || !strings.Contains(err.Error(), "ephemeral") {
		t.Fatalf("ephemeral actor entered role binding: %v", err)
	}
	_, err = s.OpenActorRequest(ctx, ActorRequestInput{SwarmID: "one", ActorUID: a.UID, ActorIncarnation: a.Incarnation, Kind: "text", Question: "A global question without a child step"})
	if !errors.Is(err, ErrSwarmRequestDenied) || !strings.Contains(err.Error(), "child-step") {
		t.Fatalf("ephemeral actor asked outside step context: %v", err)
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("ephemeral role/global request refusal changed history")
	}
	if _, err := s.db.Exec(`INSERT INTO swarm_role_bindings(swarm_id,role,actor_uid,actor_incarnation) VALUES('one','build',?,1)`, a.UID); err == nil {
		t.Fatal("raw role binding bypassed ephemeral guard")
	}
	if _, err := s.db.Exec(`UPDATE swarm_role_bindings SET actor_uid=? WHERE swarm_id='one' AND role='review'`, a.UID); err == nil {
		t.Fatal("role reassignment bypassed ephemeral guard")
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("raw role refusal changed existing persistent binding")
	}
}

func TestManagedActorClassRejectsUnknownAndInvalidEphemeralMetadata(t *testing.T) {
	s, _, a := classFixture(t)
	for _, invalid := range []map[string]string{
		{"actor_class": "'unknown'"},
		{"role": "'master'", "delegator_id": "'u'", "parent_uid": "''"},
		{"parent_uid": "''"},
		{"delegator_id": "'u'"},
	} {
		columns := strings.Split(managedActorCols, ",")
		expressions := append([]string(nil), columns...)
		overrides := map[string]string{"uid": "'invalid-class'", "name": "'invalid-class'", "ctid": "500"}
		for name, value := range invalid {
			overrides[name] = value
		}
		for index, column := range columns {
			if value, exists := overrides[column]; exists {
				expressions[index] = value
			}
		}
		query := `INSERT INTO managed_actors (` + managedActorCols + `) SELECT ` + strings.Join(expressions, ",") + ` FROM managed_actors WHERE uid=?`
		before := databaseRows(t, s.db)
		if _, err := s.db.Exec(query, a.UID); err == nil {
			t.Fatal("invalid class/delegation metadata entered inventory")
		}
		if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
			t.Fatal("invalid class constraint refusal changed rows")
		}
	}
}

func TestManagedActorClassIsImmutableAndEphemeralReservationsCountCapacity(t *testing.T) {
	s, parent, a := classFixture(t)
	ctx := context.Background()
	if a.Class != ActorClassEphemeral {
		t.Fatal("internal inventory class was lost")
	}
	for _, sql := range []string{
		`UPDATE managed_actors SET actor_class='persistent' WHERE uid='ephemeral'`,
		`UPDATE managed_actors SET actor_class='ephemeral' WHERE uid='parent'`,
		`INSERT OR REPLACE INTO managed_actors (` + managedActorCols + `) SELECT ` + strings.TrimSuffix(managedActorCols, ",actor_class") + `,'persistent' FROM managed_actors WHERE uid='ephemeral'`,
	} {
		before := databaseRows(t, s.db)
		if _, err := s.db.Exec(sql); err == nil {
			t.Fatal("actor class was rewritten")
		}
		if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
			t.Fatal("class rewrite refusal changed rows")
		}
	}
	mutated := parent
	mutated.Class = ActorClassEphemeral
	if _, err := s.UpdateManagedActor(ctx, mutated, parent.Revision); !errors.Is(err, ErrManagedBinding) {
		t.Fatal("CAS update changed class")
	}
	_, err := s.ReserveManagedActor(ctx, managedRequest("capacity-overflow", "two", 103))
	if !errors.Is(err, ErrPoolCapacity) {
		t.Fatalf("ephemeral reservation was excluded from pool capacity: %v", err)
	}
}
