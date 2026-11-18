package store

import (
	"context"
	"errors"
	"testing"
)

// FEATURE: Права владельца проекта выданы явным CAS и не выводятся из членства.
// SPEC: Admin назначает текущего active member; old/default rows не дают прав,
// owner редактирует policy без installation-admin, отзыв действует немедленно.
func TestProjectOwnershipExplicitGrantAndPolicyAccess(t *testing.T) {
	s, ctx := policyFixture(t)
	initial, err := s.ProjectOwnership(ctx, "policy-project")
	mustNil(t, err)
	if initial.Revision != 0 || initial.OwnerID != "" {
		t.Fatal("unconfigured project gained an inferred owner")
	}
	allowed, err := s.CanManageProjectWorkflowPolicy(ctx, "member", "policy-project")
	mustNil(t, err)
	if allowed {
		t.Fatal("membership inferred policy ownership")
	}
	owner, err := s.SetProjectOwner(ctx, "admin", "policy-project", 0, "member", policySession)
	mustNil(t, err)
	if owner.Revision != 1 || owner.OwnerID != "member" || owner.UpdatedBy != "admin" || owner.UpdatedAt == "" {
		t.Fatalf("missing explicit grant provenance: %+v", owner)
	}
	allowed, err = s.CanManageProjectWorkflowPolicy(ctx, "member", "policy-project")
	mustNil(t, err)
	if !allowed {
		t.Fatal("explicit owner cannot manage policy")
	}
	version, err := s.SetProjectWorkflowPolicy(ctx, "member", "policy-project", 0, policyRules(), policySession)
	mustNil(t, err)
	if version.CreatedBy != "member" {
		t.Fatal("policy edit impersonated the administrator")
	}
	user, err := s.GetUser(ctx, "member")
	mustNil(t, err)
	if user.IsAdmin {
		t.Fatal("project ownership elevated installation authority")
	}
	if _, err = s.SetProjectOwner(ctx, "member", "policy-project", 1, "backup", policySession); !errors.Is(err, ErrProjectOwnerDenied) {
		t.Fatalf("owner reassigned its grant: %v", err)
	}
	if _, err = s.SetProjectOwner(ctx, "admin", "policy-project", 0, "", policySession); !errors.Is(err, ErrProjectOwnerConflict) {
		t.Fatalf("stale ownership CAS succeeded: %v", err)
	}
	revoked, err := s.SetProjectOwner(ctx, "admin", "policy-project", 1, "", policySession)
	mustNil(t, err)
	if revoked.OwnerID != "" || revoked.Revision != 2 {
		t.Fatal("explicit revoke lost its revision")
	}
	if _, err = s.SetProjectWorkflowPolicy(ctx, "member", "policy-project", 1, nil, policySession); !errors.Is(err, ErrWorkflowPolicyDenied) {
		t.Fatalf("revoked owner retained policy permission: %v", err)
	}
}

func TestProjectOwnerGrantRequiresCurrentAdminSessionAndMembership(t *testing.T) {
	for _, change := range []string{"nil-session", "revoked-session", "disabled-admin", "demoted-admin", "removed-target", "disabled-target", "foreign-target"} {
		t.Run(change, func(t *testing.T) {
			s, ctx := policyFixture(t)
			target := "member"
			authorize := policySession
			switch change {
			case "nil-session":
				authorize = nil
			case "revoked-session":
				authorize = func(context.Context) bool { return false }
			case "disabled-admin":
				authorize = func(ctx context.Context) bool { mustNil(t, s.SetUserDisabled(ctx, "admin", true)); return true }
			case "demoted-admin":
				authorize = func(ctx context.Context) bool { mustNil(t, s.SetUserAdmin(ctx, "admin", false)); return true }
			case "removed-target":
				authorize = func(ctx context.Context) bool {
					mustNil(t, s.RemoveMember(ctx, "policy-project", "member"))
					return true
				}
			case "disabled-target":
				authorize = func(ctx context.Context) bool { mustNil(t, s.SetUserDisabled(ctx, "member", true)); return true }
			case "foreign-target":
				target = "backup"
			}
			if _, err := s.SetProjectOwner(ctx, "admin", "policy-project", 0, target, authorize); !errors.Is(err, ErrProjectOwnerDenied) {
				t.Fatalf("unsupported ownership grant: %v", err)
			}
			current, err := s.ProjectOwnership(ctx, "policy-project")
			mustNil(t, err)
			if current.Revision != 0 || current.OwnerID != "" {
				t.Fatal("denied grant changed ownership")
			}
		})
	}
}

func TestProjectOwnershipCurrentOwnerLosesPermissionWithMembershipOrAccount(t *testing.T) {
	for _, change := range []string{"membership", "disabled"} {
		t.Run(change, func(t *testing.T) {
			s, ctx := policyFixture(t)
			_, err := s.SetProjectOwner(ctx, "admin", "policy-project", 0, "member", policySession)
			mustNil(t, err)
			if change == "membership" {
				mustNil(t, s.RemoveMember(ctx, "policy-project", "member"))
			} else {
				mustNil(t, s.SetUserDisabled(ctx, "member", true))
			}
			allowed, err := s.CanManageProjectWorkflowPolicy(ctx, "member", "policy-project")
			mustNil(t, err)
			if allowed {
				t.Fatal("stale ownership granted policy authority")
			}
			if _, err := s.SetProjectWorkflowPolicy(ctx, "member", "policy-project", 0, nil, policySession); !errors.Is(err, ErrWorkflowPolicyDenied) {
				t.Fatalf("stale owner edited policy: %v", err)
			}
		})
	}
}
