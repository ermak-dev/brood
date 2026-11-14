package store

// BUG: Дрейф repository и остановленный рой блокируют отзыв и оживляют старые grants.
// HYPOTHESIS: Уменьшение authority ошибочно требует условий выдачи и использования grant.
// SOLUTION: Disable не требует совпадения repository; revoke проверяет права без active state.
// RESULT: FIXED — CAS, текущие права и неизменность истории сохранены; recovery не оживляет grant.
// STATUS: FIXED

import (
	"context"
	"errors"
	"testing"
)

func TestGitLabDisableBindingAfterRepositoryDrift(t *testing.T) {
	for _, repository := range []string{"https://git.example/group/other.git", ""} {
		t.Run(repository, func(t *testing.T) {
			ctx := context.Background()
			s := newTestStore(t)
			p, b := gitlabFixture(t, s)
			gitlabGrant(t, s, p, b)
			_, err := s.CheckGitLabAccess(ctx, p, "read")
			mustNil(t, err)
			mustNil(t, s.SetProjectRepository(ctx, p.ProjectID, repository))
			gitlabDeniedUnchanged(t, s, func() error { _, e := s.CheckGitLabAccess(ctx, p, "read"); return e })
			// Включение по-прежнему требует точного текущего repository.
			gitlabDeniedUnchanged(t, s, func() error { _, e := s.PutGitLabBinding(ctx, "admin", b, b.Revision); return e })
			b.Enabled = false
			gitlabDeniedUnchanged(t, s, func() error { _, e := s.PutGitLabBinding(ctx, "u", b, b.Revision); return e })
			seedUser(t, s, "other-admin", "other-admin")
			mustNil(t, s.SetUserAdmin(ctx, "other-admin", true))
			mustNil(t, s.SetUserDisabled(ctx, "admin", true))
			gitlabDeniedUnchanged(t, s, func() error { _, e := s.PutGitLabBinding(ctx, "admin", b, b.Revision); return e })
			mustNil(t, s.SetUserDisabled(ctx, "admin", false))
			_, err = s.PutGitLabBinding(ctx, "admin", b, 0)
			if !errors.Is(err, ErrGitLabConflict) {
				t.Errorf("stale disable must conflict despite drift: %v", err)
			}
			before := b
			b, err = s.PutGitLabBinding(ctx, "admin", b, b.Revision)
			if err != nil {
				t.Fatalf("authorized disable after repository drift: %v", err)
			}
			before.Enabled = false
			before.Revision++
			if b != before {
				t.Fatalf("disable changed binding identity: %+v want %+v", b, before)
			}
			stored, err := s.GetGitLabBinding(ctx, p.ProjectID)
			mustNil(t, err)
			if stored != b {
				t.Fatalf("disable was not persisted: %+v", stored)
			}
			mustNil(t, s.SetProjectRepository(ctx, p.ProjectID, b.Repository))
			gitlabDeniedUnchanged(t, s, func() error { _, e := s.CheckGitLabAccess(ctx, p, "read"); return e })
			b.Enabled = true
			_, err = s.PutGitLabBinding(ctx, "admin", b, b.Revision)
			mustNil(t, err)
			gitlabDeniedUnchanged(t, s, func() error { _, e := s.CheckGitLabAccess(ctx, p, "read"); return e })
		})
	}
}

func TestGitLabRevokeGrantDuringOutage(t *testing.T) {
	for _, human := range []string{"admin", "u"} {
		for _, state := range []string{"inactive", "error", "actor stopped", "actor error"} {
			t.Run(human+"/"+state, func(t *testing.T) {
				ctx := context.Background()
				s := newTestStore(t)
				p, b := gitlabFixture(t, s)
				g := gitlabGrant(t, s, p, b)
				_, err := s.CheckGitLabAccess(ctx, p, "read")
				mustNil(t, err)
				switch state {
				case "actor stopped":
					setActor(t, s, p.ActorUID, "phase='stopped',desired='stopped'")
				case "actor error":
					setActor(t, s, p.ActorUID, "phase='error'")
				default:
					mustNil(t, s.SetSwarmState(ctx, p.SwarmID, state))
				}
				gitlabDeniedUnchanged(t, s, func() error { _, e := s.CheckGitLabAccess(ctx, p, "read"); return e })
				if err := s.RevokeGitLabGrant(ctx, human, g.ID, 0); !errors.Is(err, ErrGitLabConflict) {
					t.Errorf("stale revoke must conflict during outage: %v", err)
				}
				if err := s.RevokeGitLabGrant(ctx, human, g.ID, g.Revision); err != nil {
					t.Fatalf("authorized revoke during %s: %v", state, err)
				}
				stored, err := s.GetGitLabGrant(ctx, g.ID)
				mustNil(t, err)
				g.Revoked = true
				g.Revision++
				if stored != g {
					t.Fatalf("revoke changed immutable grant identity: %+v want %+v", stored, g)
				}
				gitlabDeniedUnchanged(t, s, func() error { return s.RevokeGitLabGrant(ctx, human, g.ID, g.Revision) })
				mustNil(t, s.SetSwarmState(ctx, p.SwarmID, "active"))
				setActor(t, s, p.ActorUID, settledRunning)
				gitlabDeniedUnchanged(t, s, func() error { _, e := s.CheckGitLabAccess(ctx, p, "read"); return e })
			})
		}
	}
}

func TestGitLabRevokeOutagePreservesHumanScope(t *testing.T) {
	for _, change := range []string{"disabled admin", "disabled owner", "demoted admin", "editor", "viewer", "no project membership", "foreign owner"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			s := newTestStore(t)
			p, b := gitlabFixture(t, s)
			g := gitlabGrant(t, s, p, b)
			mustNil(t, s.SetSwarmState(ctx, p.SwarmID, "error"))
			seedUser(t, s, "other-admin", "other-admin")
			mustNil(t, s.SetUserAdmin(ctx, "other-admin", true))
			human := "u"
			switch change {
			case "disabled admin":
				human = "admin"
				mustNil(t, s.SetUserDisabled(ctx, human, true))
			case "disabled owner":
				mustNil(t, s.SetUserDisabled(ctx, human, true))
			case "demoted admin":
				human = "admin"
				mustNil(t, s.SetUserAdmin(ctx, human, false))
			case "editor", "viewer":
				_, err := s.db.Exec(`UPDATE swarm_members SET role=? WHERE swarm_id=? AND user_id=?`, change, p.SwarmID, human)
				mustNil(t, err)
			case "no project membership":
				mustNil(t, s.RemoveMember(ctx, p.ProjectID, human))
			case "foreign owner":
				seedUser(t, s, "foreign", "foreign")
				human = "foreign"
				mustNil(t, s.AddMember(ctx, "two", human))
				_, err := s.db.Exec(`INSERT INTO swarm_members(swarm_id,user_id,role) VALUES ('two',?,'owner')`, human)
				mustNil(t, err)
			}
			gitlabDeniedUnchanged(t, s, func() error {
				err := s.RevokeGitLabGrant(ctx, human, g.ID, g.Revision)
				if !errors.Is(err, ErrGitLabDenied) {
					t.Fatalf("unauthorized revoke: %v", err)
				}
				return err
			})
		})
	}
}
