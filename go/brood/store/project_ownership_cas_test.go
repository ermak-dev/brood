package store

import (
	"errors"
	"testing"
)

// SPEC: Ownership CAS признаёт ровно одну запись и проверяет сохранённый grant,
// даже если SQLite trigger проигнорировал или уничтожил запись после Exec.
func TestProjectOwnerCASConcurrentOnlyOneGrant(t *testing.T) {
	s, ctx := policyFixture(t)
	seedUser(t, s, "second", "second")
	mustNil(t, s.AddMember(ctx, "policy-project", "second"))
	type result struct {
		grant ProjectOwnership
		err   error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for _, username := range []string{"member", "second"} {
		go func() {
			<-start
			grant, err := s.SetProjectOwner(ctx, "admin", "policy-project", 0, username, policySession)
			results <- result{grant, err}
		}()
	}
	close(start)
	wins, conflicts := 0, 0
	var winner ProjectOwnership
	for range 2 {
		result := <-results
		if result.err == nil {
			wins++
			winner = result.grant
		} else if errors.Is(result.err, ErrProjectOwnerConflict) {
			conflicts++
		} else {
			t.Fatal(result.err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("ownership CAS wins=%d conflicts=%d", wins, conflicts)
	}
	current, err := s.ProjectOwnership(ctx, "policy-project")
	mustNil(t, err)
	if current != winner || current.Revision != 1 {
		t.Fatal("ownership CAS returned a grant other than its committed winner")
	}
}

func TestProjectOwnerIgnoredOrDeletedWriteCannotAcknowledgeGrant(t *testing.T) {
	for _, mode := range []string{"ignore-update", "delete-after-insert"} {
		t.Run(mode, func(t *testing.T) {
			s, ctx := policyFixture(t)
			expected := int64(0)
			username := "member"
			before, err := s.ProjectOwnership(ctx, "policy-project")
			mustNil(t, err)
			if mode == "ignore-update" {
				before, err = s.SetProjectOwner(ctx, "admin", "policy-project", 0, "member", policySession)
				mustNil(t, err)
				expected = 1
				username = ""
				_, err = s.db.Exec(`CREATE TRIGGER owner_write_ignored BEFORE UPDATE ON project_ownership BEGIN SELECT RAISE(IGNORE); END`)
			} else {
				_, err = s.db.Exec(`CREATE TRIGGER owner_write_deleted AFTER INSERT ON project_ownership BEGIN DELETE FROM project_ownership WHERE project_id=NEW.project_id; END`)
			}
			mustNil(t, err)
			if _, err = s.SetProjectOwner(ctx, "admin", "policy-project", expected, username, policySession); !errors.Is(err, ErrProjectOwnerConflict) {
				t.Fatalf("unretained ownership grant acknowledged: %v", err)
			}
			after, err := s.ProjectOwnership(ctx, "policy-project")
			mustNil(t, err)
			if before != after {
				t.Fatalf("failed ownership write changed grant: before=%+v after=%+v", before, after)
			}
		})
	}
}
