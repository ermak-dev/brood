package store

// FEATURE: Версии workflow policy неизменяемы, current обновляется по CAS (#290).
// SPEC: Только текущий admin с действующей сессией; отсутствие policy не выдумывает
// правила; удаление проекта не стирает версии, policy пока не включает gate engine.

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"brood.dev/go/brood/workflowpolicy"
)

func policyFixture(t *testing.T) (*Store, context.Context) {
	t.Helper()
	s := newTestStore(t)
	ctx := context.Background()
	seedUser(t, s, "admin", "admin")
	seedUser(t, s, "backup", "backup")
	seedUser(t, s, "member", "member")
	mustNil(t, s.SetUserAdmin(ctx, "admin", true))
	mustNil(t, s.SetUserAdmin(ctx, "backup", true))
	seedProject(t, s, "policy-project", "policy-project")
	mustNil(t, s.AddMember(ctx, "policy-project", "member"))
	return s, ctx
}
func policyRules() []workflowpolicy.Rule {
	return []workflowpolicy.Rule{{ID: "storage", Paths: []string{"go/brood/store/**"}, Require: workflowpolicy.Human}}
}
func policySession(context.Context) bool { return true }

func TestWorkflowPolicyVersionCASAndHistoricalReads(t *testing.T) {
	s, ctx := policyFixture(t)
	initial, err := s.ProjectWorkflowPolicy(ctx, "policy-project")
	mustNil(t, err)
	if initial.Revision != 0 || len(initial.Rules) != 0 {
		t.Fatal("new project inherited policy")
	}
	rules := policyRules()
	first, err := s.SetProjectWorkflowPolicy(ctx, "admin", "policy-project", 0, rules, policySession)
	mustNil(t, err)
	if first.Revision != 1 || first.CreatedBy != "admin" || first.CreatedAt == "" {
		t.Fatalf("missing version provenance: %+v", first)
	}
	rules[0].Paths[0] = "docs/**"
	second, err := s.SetProjectWorkflowPolicy(ctx, "admin", "policy-project", 1, nil, policySession)
	mustNil(t, err)
	if second.Revision != 2 || len(second.Rules) != 0 {
		t.Fatal("explicit empty policy not versioned")
	}
	historical, err := s.ProjectWorkflowPolicyVersion(ctx, "policy-project", 1)
	mustNil(t, err)
	if !reflect.DeepEqual(historical.Rules, policyRules()) {
		t.Fatal("history changed after policy replacement or input mutation")
	}
	if _, err := s.SetProjectWorkflowPolicy(ctx, "admin", "policy-project", 1, policyRules(), policySession); !errors.Is(err, ErrWorkflowPolicyConflict) {
		t.Fatalf("stale update: %v", err)
	}
	current, err := s.ProjectWorkflowPolicy(ctx, "policy-project")
	mustNil(t, err)
	if current.Revision != 2 {
		t.Fatal("stale update changed head")
	}
}

func TestWorkflowPolicyAuthorityIsCurrentAndTransactional(t *testing.T) {
	for _, tc := range []struct {
		name, actor string
		authorize   func(*Store, context.Context) bool
	}{
		{"member", "member", func(*Store, context.Context) bool { return true }},
		{"missing session", "admin", nil},
		{"revoked session", "admin", func(*Store, context.Context) bool { return false }},
		{"demoted during authorization", "admin", func(s *Store, ctx context.Context) bool { mustNil(t, s.SetUserAdmin(ctx, "admin", false)); return true }},
		{"disabled during authorization", "admin", func(s *Store, ctx context.Context) bool {
			mustNil(t, s.SetUserDisabled(ctx, "admin", true))
			return true
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ctx := policyFixture(t)
			var authorize func(context.Context) bool
			if tc.authorize != nil {
				authorize = func(ctx context.Context) bool { return tc.authorize(s, ctx) }
			}
			if _, err := s.SetProjectWorkflowPolicy(ctx, tc.actor, "policy-project", 0, policyRules(), authorize); !errors.Is(err, ErrWorkflowPolicyDenied) {
				t.Fatalf("unauthorized edit: %v", err)
			}
			current, err := s.ProjectWorkflowPolicy(ctx, "policy-project")
			mustNil(t, err)
			if current.Revision != 0 {
				t.Fatal("denied update persisted")
			}
		})
	}
}

func TestWorkflowPolicyConcurrentCASHasOneWinner(t *testing.T) {
	s, ctx := policyFixture(t)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.SetProjectWorkflowPolicy(ctx, "admin", "policy-project", 0, policyRules(), policySession)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	wins, conflicts := 0, 0
	for err := range results {
		if err == nil {
			wins++
		} else if errors.Is(err, ErrWorkflowPolicyConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
}

func TestWorkflowPolicyHistoryCannotBeRewrittenOrErased(t *testing.T) {
	s, ctx := policyFixture(t)
	_, err := s.SetProjectWorkflowPolicy(ctx, "admin", "policy-project", 0, policyRules(), policySession)
	mustNil(t, err)
	for _, query := range []string{
		`UPDATE project_workflow_policy_versions SET created_by='other'`,
		`DELETE FROM project_workflow_policy_versions`,
		`INSERT OR REPLACE INTO project_workflow_policy_versions SELECT * FROM project_workflow_policy_versions`,
	} {
		if _, err := s.db.Exec(query); err == nil || !strings.Contains(err.Error(), "workflow policy version is immutable") {
			t.Fatalf("history fence: %v", err)
		}
	}
	mustNil(t, s.DeleteProject(ctx, "policy-project"))
	old, err := s.ProjectWorkflowPolicyVersion(ctx, "policy-project", 1)
	mustNil(t, err)
	if !reflect.DeepEqual(old.Rules, policyRules()) {
		t.Fatal("project deletion erased policy history")
	}
	if _, err := s.ProjectWorkflowPolicy(ctx, "policy-project"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing project has current policy: %v", err)
	}
	requireIntact(t, s.db)
}

func TestWorkflowPolicyCurrentPointerCannotRollBackOrDisappear(t *testing.T) {
	s, ctx := policyFixture(t)
	_, err := s.SetProjectWorkflowPolicy(ctx, "admin", "policy-project", 0, policyRules(), policySession)
	mustNil(t, err)
	_, err = s.SetProjectWorkflowPolicy(ctx, "admin", "policy-project", 1, nil, policySession)
	mustNil(t, err)
	for _, query := range []string{
		`UPDATE project_workflow_policy_heads SET revision=1`,
		`DELETE FROM project_workflow_policy_heads`,
		`INSERT OR REPLACE INTO project_workflow_policy_heads VALUES ('policy-project',1)`,
	} {
		if _, err := s.db.Exec(query); err == nil || !strings.Contains(err.Error(), "workflow policy head requires a new revision") {
			t.Fatalf("current policy pointer was rewound or erased: %v", err)
		}
	}
	mustNil(t, s.DeleteProject(ctx, "policy-project"))
	requireIntact(t, s.db)
}

func TestWorkflowPolicyRejectsInvalidRulesWithoutWriting(t *testing.T) {
	s, ctx := policyFixture(t)
	for _, rules := range [][]workflowpolicy.Rule{
		{{ID: "invalid", Paths: []string{"../secret"}, Require: workflowpolicy.Human}},
		{{ID: "invalid", Actions: []string{"landing"}, Require: "allow"}},
	} {
		if _, err := s.SetProjectWorkflowPolicy(ctx, "admin", "policy-project", 0, rules, policySession); err == nil {
			t.Fatal("invalid rules persisted")
		}
	}
	current, err := s.ProjectWorkflowPolicy(ctx, "policy-project")
	mustNil(t, err)
	if current.Revision != 0 {
		t.Fatal("invalid update changed current policy")
	}
}
