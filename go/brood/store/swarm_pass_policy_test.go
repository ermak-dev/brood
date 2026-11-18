package store

// FEATURE: Проход фиксирует policy в транзакции старта (#290).
// SPEC: Правки default не меняют текущий/старый проход; отсутствие policy явно
// закрепляет пустую конфигурацию. Snapshot неизменяем и не читается из default.

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"brood.dev/go/brood/workflowpolicy"
)

func TestPassWorkflowPolicyRemainsPinnedAcrossEditsAndRestart(t *testing.T) {
	ctx := context.Background()
	s := goalStore(t)
	first, err := s.SetProjectWorkflowPolicy(ctx, "root", "one", 0, policyRules(), allow)
	mustNil(t, err)
	mustStart(t, s, "release")
	pin, err := s.SwarmStepWorkflowPolicy(ctx, "one", 1)
	mustNil(t, err)
	if pin.Pass != 1 || pin.Revision != first.Revision || pin.ProjectID != "one" || pin.LegacyEmpty || !reflect.DeepEqual(pin.Rules, policyRules()) {
		t.Fatalf("wrong pass snapshot: %+v", pin)
	}
	_, err = s.SetProjectWorkflowPolicy(ctx, "root", "one", 1, []workflowpolicy.Rule{{ID: "deny-land", Actions: []string{"landing"}, Require: workflowpolicy.Deny}}, allow)
	mustNil(t, err)
	answerNode(t, s, 1, "success")
	secondStep, err := s.SwarmStepWorkflowPolicy(ctx, "one", 2)
	mustNil(t, err)
	if !reflect.DeepEqual(secondStep, pin) {
		t.Fatal("policy edit changed current pass")
	}
	stopOne(t, s)
	mustStart(t, s, "release")
	next, err := s.SwarmStepWorkflowPolicy(ctx, "one", 3)
	mustNil(t, err)
	if next.Pass != 3 || next.Revision != 2 || len(next.Rules) != 1 || next.Rules[0].ID != "deny-land" {
		t.Fatalf("new pass missed policy update: %+v", next)
	}
	old, err := s.SwarmStepWorkflowPolicy(ctx, "one", 1)
	mustNil(t, err)
	if !reflect.DeepEqual(old, pin) {
		t.Fatal("restart rewrote old pass policy")
	}
	if _, err := s.SwarmStepWorkflowPolicy(ctx, "two", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign step inherited snapshot: %v", err)
	}
}

func TestPassWithoutPolicyDoesNotInheritLaterDefault(t *testing.T) {
	ctx := context.Background()
	s := goalStore(t)
	mustStart(t, s, "release")
	_, err := s.SetProjectWorkflowPolicy(ctx, "root", "one", 0, policyRules(), allow)
	mustNil(t, err)
	answerNode(t, s, 1, "success")
	pin, err := s.SwarmStepWorkflowPolicy(ctx, "one", 2)
	mustNil(t, err)
	if pin.Revision != 0 || pin.LegacyEmpty || len(pin.Rules) != 0 {
		t.Fatalf("empty pass inherited current policy: %+v", pin)
	}
}

func TestPassWorkflowPolicyCannotBeChangedOrErased(t *testing.T) {
	ctx := context.Background()
	s := goalStore(t)
	mustStart(t, s, "release")
	for _, query := range []string{
		`UPDATE swarm_pass_policies SET legacy_empty=1`,
		`DELETE FROM swarm_pass_policies`,
		`INSERT OR REPLACE INTO swarm_pass_policies SELECT * FROM swarm_pass_policies`,
	} {
		if _, err := s.db.Exec(query); err == nil || !strings.Contains(err.Error(), "swarm pass policy is immutable") {
			t.Fatalf("snapshot fence: %v", err)
		}
	}
	before := dumpTables(t, s, "swarm_pass_policies")
	if _, err := s.StartSwarm(ctx, SwarmStart{SwarmID: "one", UserID: "u", Definition: "release", Commit: startCommit, Goal: startGoal}, func(context.Context) bool { return false }); !errors.Is(err, ErrSwarmRequestDenied) {
		t.Fatalf("revoked start: %v", err)
	}
	if dumpTables(t, s, "swarm_pass_policies") != before {
		t.Fatal("failed start changed policy history")
	}
}
