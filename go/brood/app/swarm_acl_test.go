package app

import (
	"errors"
	"testing"
)

// FEATURE: Explicit swarm ACL roles, with project membership as a prerequisite (#47).
// SPEC: Viewers read, editors manage workloads, owners manage grants/delete/transfer;
// membership alone never grants a swarm and disabled users are always refused.
// IMPLEMENTATION: Explicit grants drive Can and listing; gateway writes revalidate atomically.
// RESULT: IMPLEMENTED — domain matrix, revocation and real-store stale-write tests pass.
func TestSwarmACLRoleMatrix(t *testing.T) {
	for _, role := range []string{"", "viewer", "editor", "owner"} {
		t.Run("role="+role, func(t *testing.T) {
			e := newEnv(t)
			e.seedUsers(t)
			project := e.seedProject(t, idAlice, idBob)
			sw := mustCreateSwarm(t, e, idAlice, project, "alpha")
			if role != "" {
				if err := e.svc.SetSwarmMember(idAlice, sw.ID, idBob, role); err != nil {
					t.Fatal(err)
				}
			}
			for _, tc := range []struct {
				action Action
				allow  bool
			}{
				{ActionViewSwarm, role != ""},
				{ActionManageSwarm, role == "owner" || role == "editor"},
				{ActionDeleteSwarm, role == "owner"},
				{ActionTransferOwnership, role == "owner"},
				{ActionManageSwarmACL, role == "owner"},
			} {
				got, err := e.svc.Can(idBob, tc.action, SwarmRes(sw.ID))
				if err != nil || got != tc.allow {
					t.Fatalf("Can(%s, %s) = %v, %v, want %v", role, tc.action, got, err, tc.allow)
				}
			}
			visible, err := e.svc.ListSwarmsFor(idBob, project)
			if err != nil || (len(visible) == 1) != (role != "") {
				t.Fatalf("visible swarms for %q = %+v, %v", role, visible, err)
			}
			if role == "" {
				return
			}
			if err := e.svc.RemoveMember(idAdmin, project, idBob); err != nil {
				t.Fatal(err)
			}
			if allowed, err := e.svc.Can(idBob, ActionViewSwarm, SwarmRes(sw.ID)); err != nil || allowed {
				t.Fatalf("grant without project membership allowed: %v, %v", allowed, err)
			}
			if err := e.svc.AddMember(idAdmin, project, idBob); err != nil {
				t.Fatal(err)
			}
			if err := e.svc.SetUserDisabled(idAdmin, idBob, true); err != nil {
				t.Fatal(err)
			}
			if allowed, err := e.svc.Can(idBob, ActionViewSwarm, SwarmRes(sw.ID)); err != nil || allowed {
				t.Fatalf("disabled grantee allowed: %v, %v", allowed, err)
			}
		})
	}
}

func TestSwarmACLGrantAdministration(t *testing.T) {
	e := newEnv(t)
	e.seedUsers(t)
	project := e.seedProject(t, idAlice, idBob)
	sw := mustCreateSwarm(t, e, idAlice, project, "alpha")
	if err := e.svc.SetSwarmMember(idAlice, sw.ID, idBob, "editor"); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"owner", "viewer"} {
		if err := e.svc.SetSwarmMember(idBob, sw.ID, idBob, role); !errors.Is(err, ErrNotAllowed) {
			t.Fatalf("editor changed own role: %v", err)
		}
	}
	if err := e.svc.RemoveSwarmMember(idBob, sw.ID, idAlice); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("editor revoked owner: %v", err)
	}
	if err := e.svc.SetSwarmMember(idAlice, sw.ID, idBob, "ownre"); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("invalid role error = %v", err)
	}
	if err := e.svc.SetSwarmMember(idAlice, sw.ID, idAlice, "viewer"); !errors.Is(err, ErrConflict) {
		t.Fatalf("last owner demotion = %v", err)
	}
	if err := e.svc.RemoveSwarmMember(idAlice, sw.ID, idAlice); !errors.Is(err, ErrConflict) {
		t.Fatalf("last owner removal = %v", err)
	}
	if err := e.svc.RemoveSwarmMember(idAlice, sw.ID, idBob); err != nil {
		t.Fatal(err)
	}
	if allowed, err := e.svc.Can(idBob, ActionViewSwarm, SwarmRes(sw.ID)); err != nil || allowed {
		t.Fatalf("revoked grant still works: %v, %v", allowed, err)
	}
	if err := e.svc.RemoveMember(idAdmin, project, idBob); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetSwarmMember(idAlice, sw.ID, idBob, "viewer"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("grant to nonmember = %v", err)
	}
}

func TestSwarmOwnerCanTransferAndCoOwnerCanDelete(t *testing.T) {
	e := newEnv(t)
	e.seedUsers(t)
	project := e.seedProject(t, idAlice, idBob)
	sw := mustCreateSwarm(t, e, idAlice, project, "alpha")
	if err := e.svc.TransferSwarm(idAlice, sw.ID, idBob); err != nil {
		t.Fatalf("owner transfer: %v", err)
	}
	if allowed, _ := e.svc.Can(idAlice, ActionViewSwarm, SwarmRes(sw.ID)); allowed {
		t.Fatal("old owner retained implicit access")
	}
	if err := e.svc.SetSwarmMember(idBob, sw.ID, idAlice, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeleteSwarm(idAlice, sw.ID); err != nil {
		t.Fatalf("co-owner cannot delete: %v", err)
	}
}
