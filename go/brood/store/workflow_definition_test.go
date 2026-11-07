package store

// FEATURE: Project-scoped versioned workflow definitions.
// SPEC: Atomic validation and authorization, unchanged syncs do not write, and projects remain isolated.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"brood.dev/go/brood/pipeline"
)

const workflowYAML = `apiVersion: brood.dev/v1alpha1
name: demo
start: s
exit: e
nodes:
  s: {type: actor, prompt: go}
  l: {type: land, target: main}
  e: {type: exit}
edges:
  - {from: s, to: l}
  - {from: l, to: e}
goalGates: [l]
`

// BUG: Rejected callers can force workflow graph validation.
// HYPOTHESIS: Authorization occurs only after parsing the supplied definitions.
// SOLUTION: Check session and project access before validation and again in the write transaction.
// RESULT: FIXED — rejected callers cannot trigger graph validation or write after revocation.
// STATUS: FIXED
func TestWorkflowSyncAuthorizationBeforeValidation(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedProjectWithMember(t, s, "p", "member")
	seedProjectWithMember(t, s, "other", "outsider")
	if err := s.CreateUser(ctx, User{ID: "admin", Username: "admin", PasswordHash: "h", IsAdmin: true}); err != nil {
		t.Fatal(err)
	}
	yes := func(context.Context) bool { return true }
	no := func(context.Context) bool { return false }
	for _, tc := range []struct {
		name, actor, text string
		authorize         func(context.Context) bool
	}{
		{"denied malformed", "admin", "invalid: [", no},
		{"denied oversized", "admin", strings.Repeat("x", pipeline.MaxWorkflowDefinitionBytes+1), no},
		{"nil callback", "admin", "invalid: [", nil},
		{"outsider", "outsider", "invalid: [", yes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.SyncWorkflowDefinitions(ctx, "p", tc.actor, map[string]string{"demo": tc.text}, tc.authorize)
			if !errors.Is(err, ErrProjectSyncDenied) {
				t.Fatalf("expected authorization denial before validation, got %v", err)
			}
		})
	}
	for _, text := range []string{"invalid: [", strings.Repeat("x", pipeline.MaxWorkflowDefinitionBytes+1)} {
		_, err := s.SyncWorkflowDefinitions(ctx, "p", "admin", map[string]string{"demo": text}, yes)
		if err == nil || errors.Is(err, ErrProjectSyncDenied) {
			t.Fatalf("authorized invalid input must retain validation error, got %v", err)
		}
	}
	calls := 0
	_, err := s.SyncWorkflowDefinitions(ctx, "p", "admin", map[string]string{"demo": workflowYAML}, func(context.Context) bool {
		calls++
		return calls == 1
	})
	if !errors.Is(err, ErrProjectSyncDenied) || calls != 2 {
		t.Fatalf("revoked session: calls=%d err=%v", calls, err)
	}
	if _, err := s.GetWorkflowDefinition(ctx, "p", "demo"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked session wrote definition: %v", err)
	}
}

// BUG: Separate workflow batches can exceed the project definition limit.
// HYPOTHESIS: Only the incoming batch is bounded, not the merged project state.
// SOLUTION: Bound the merged definitions within the write transaction.
// RESULT: FIXED — rejected batches leave the existing definitions untouched.
// STATUS: FIXED
func TestWorkflowSyncLimitsMergedDefinitions(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedProject(t, s, "p", "project")
	if err := s.CreateUser(ctx, User{ID: "admin", Username: "admin", PasswordHash: "h", IsAdmin: true}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < pipeline.MaxWorkflowDefinitions; i++ {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO workflow_definitions(project_id,name,yaml,version) VALUES ('p',?,?,1)`, fmt.Sprintf("old%d", i), workflowYAML); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.SyncWorkflowDefinitions(ctx, "p", "admin", map[string]string{"demo": workflowYAML}, func(context.Context) bool { return true })
	if err == nil {
		t.Fatal("sync accepted a definition exceeding the merged project limit")
	}
	if _, err := s.GetWorkflowDefinition(ctx, "p", "demo"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected definition was written: %v", err)
	}
}

// FEATURE: Explicit workflow deletion.
// SPEC: Only an authorized admin may delete, and any project run blocks deletion.
// IMPLEMENTATION: Authorize, check project runs, and delete in one transaction.
// RESULT: IMPLEMENTED — completed runs also block deletion.
// STATUS: DONE
func TestWorkflowDefinitionDelete(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedProjectWithMember(t, s, "p", "member")
	if err := s.CreateUser(ctx, User{ID: "admin", Username: "admin", PasswordHash: "h", IsAdmin: true}); err != nil {
		t.Fatal(err)
	}
	yes := func(context.Context) bool { return true }
	if _, err := s.SyncWorkflowDefinitions(ctx, "p", "admin", map[string]string{"demo": workflowYAML}, yes); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteWorkflowDefinition(ctx, "p", "member", "demo", yes); !errors.Is(err, ErrProjectSyncDenied) {
		t.Fatalf("member deletion: %v", err)
	}
	if err := s.DeleteWorkflowDefinition(ctx, "p", "admin", "demo", func(context.Context) bool { return false }); !errors.Is(err, ErrProjectSyncDenied) {
		t.Fatalf("revoked session: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO swarms(id,project_id,owner_id,name,atespace_name,created_at,updated_at) VALUES ('s','p','admin','s','s','now','now'); INSERT INTO pipeline_runs(id,swarm_id,owner_id,graph,status) VALUES ('r','s','admin',X'01','succeeded')`); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteWorkflowDefinition(ctx, "p", "admin", "demo", yes); err == nil || !strings.Contains(err.Error(), "pipeline runs") {
		t.Fatalf("historical run must block deletion: %v", err)
	}
	if _, err := s.GetWorkflowDefinition(ctx, "p", "demo"); err != nil {
		t.Fatalf("blocked deletion removed definition: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM pipeline_runs WHERE id='r'`); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteWorkflowDefinition(ctx, "p", "admin", "demo", yes); err != nil {
		t.Fatalf("admin deletion: %v", err)
	}
	if _, err := s.GetWorkflowDefinition(ctx, "p", "demo"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("definition remains: %v", err)
	}
}

func TestWorkflowDefinitionsSync(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedProjectWithMember(t, s, "p", "member")
	seedProject(t, s, "other", "other")
	if err := s.CreateUser(ctx, User{ID: "admin", Username: "admin", PasswordHash: "h", IsAdmin: true}); err != nil {
		t.Fatal(err)
	}
	yes := func(context.Context) bool { return true }
	changed, err := s.SyncWorkflowDefinitions(ctx, "p", "admin", map[string]string{"demo": workflowYAML}, yes)
	if err != nil || !changed {
		t.Fatalf("create: %v %v", changed, err)
	}
	d, err := s.GetWorkflowDefinition(ctx, "p", "demo")
	if err != nil || d.YAML != workflowYAML || d.Version != 1 {
		t.Fatalf("get: %+v %v", d, err)
	}
	changed, err = s.SyncWorkflowDefinitions(ctx, "p", "member", map[string]string{"demo": workflowYAML}, yes)
	if err != nil || changed {
		t.Fatalf("unchanged member: %v %v", changed, err)
	}
	_, err = s.SyncWorkflowDefinitions(ctx, "p", "member", map[string]string{"demo": workflowYAML + "\n"}, yes)
	if !errors.Is(err, ErrProjectSyncDenied) {
		t.Fatalf("member change: %v", err)
	}
	_, err = s.SyncWorkflowDefinitions(ctx, "p", "admin", map[string]string{"demo": workflowYAML, "wrong": workflowYAML}, yes)
	if err == nil {
		t.Fatal("mismatched name accepted")
	}
	d, err = s.GetWorkflowDefinition(ctx, "p", "demo")
	if err != nil || d.Version != 1 {
		t.Fatalf("atomic rejection: %+v %v", d, err)
	}
	if _, err = s.GetWorkflowDefinition(ctx, "other", "demo"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("scope: %v", err)
	}
	if _, err = s.SyncWorkflowDefinitions(ctx, "p", "admin", nil, func(context.Context) bool { return false }); !errors.Is(err, ErrProjectSyncDenied) {
		t.Fatalf("session: %v", err)
	}
	if changed, err = s.SyncWorkflowDefinitions(ctx, "p", "admin", map[string]string{"demo": workflowYAML + "\n"}, yes); err != nil || !changed {
		t.Fatalf("update: %v %v", changed, err)
	}
	d, err = s.GetWorkflowDefinition(ctx, "p", "demo")
	if err != nil || d.Version != 2 {
		t.Fatalf("version: %+v %v", d, err)
	}
	list, err := s.ListWorkflowDefinitions(ctx, "p")
	if err != nil || len(list) != 1 || list[0] != d {
		t.Fatalf("list: %+v %v", list, err)
	}
	if changed, err = s.SyncWorkflowDefinitions(ctx, "p", "member", nil, yes); err != nil || changed {
		t.Fatalf("empty: %v %v", changed, err)
	}
	if _, err = s.SyncWorkflowDefinitions(ctx, "other", "member", nil, yes); !errors.Is(err, ErrProjectSyncDenied) {
		t.Fatalf("outsider: %v", err)
	}
}

// IMPLEMENTATION: Store current YAML with a per-name revision in a transaction.
// RESULT: IMPLEMENTED — authorization and validation precede writes.
// STATUS: DONE
// FEATURE: Publication timestamps for workflow versions.
// SPEC: Migrated rows retain their data and unknown time; unchanged sync preserves it.
func TestWorkflowPublicationTimeMigration(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedProject(t, s, "p", "preserved")
	if err := s.CreateUser(ctx, User{ID: "admin", Username: "admin", PasswordHash: "h", IsAdmin: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `DROP TABLE workflow_revision_counters; DROP TABLE workflow_definitions; CREATE TABLE workflow_definitions (project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE, name TEXT NOT NULL, yaml TEXT NOT NULL, version INTEGER NOT NULL CHECK (version > 0), PRIMARY KEY (project_id, name)); INSERT INTO workflow_definitions VALUES ('p','demo',?,3); PRAGMA user_version=12`, workflowYAML); err != nil {
		t.Fatal(err)
	}
	if err := s.initializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	var text string
	var version int
	var published sql.NullString
	read := func() {
		t.Helper()
		if err := s.db.QueryRowContext(ctx, `SELECT yaml,version,published_at FROM workflow_definitions WHERE project_id='p' AND name='demo'`).Scan(&text, &version, &published); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if text != workflowYAML || version != 3 || published.Valid {
		t.Fatalf("migrated: %q %d %+v", text, version, published)
	}
	yes := func(context.Context) bool { return true }
	if _, err := s.SyncWorkflowDefinitions(ctx, "p", "admin", map[string]string{"demo": workflowYAML}, yes); err != nil {
		t.Fatal(err)
	}
	read()
	if published.Valid || version != 3 {
		t.Fatal("unchanged legacy record was rewritten")
	}
	if _, err := s.SyncWorkflowDefinitions(ctx, "p", "admin", map[string]string{"demo": workflowYAML + "\n"}, yes); err != nil {
		t.Fatal(err)
	}
	read()
	if !published.Valid || version != 4 {
		t.Fatalf("changed: %d %+v", version, published)
	}
}

func TestWorkflowVersionMigrationAndRecreate(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedProjectWithMember(t, s, "p", "member")
	if err := s.CreateUser(ctx, User{ID: "admin", Username: "admin", PasswordHash: "h", IsAdmin: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `DROP TABLE workflow_revision_counters; ALTER TABLE workflow_definitions DROP COLUMN revision; INSERT INTO workflow_definitions(project_id,name,yaml,version,published_at) VALUES ('p','demo',?,7,'then'); PRAGMA user_version=13`, workflowYAML); err != nil {
		t.Fatal(err)
	}
	if err := s.initializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	d, err := s.GetWorkflowDefinition(ctx, "p", "demo")
	if err != nil || d.Version != 7 || d.YAML != workflowYAML || d.PublishedAt.String != "then" {
		t.Fatalf("migration lost data: %+v %v", d, err)
	}
	yes := func(context.Context) bool { return true }
	if changed, err := s.SyncWorkflowDefinitions(ctx, "p", "member", map[string]string{"demo": workflowYAML}, yes); err != nil || changed {
		t.Fatalf("idempotent sync: %v %v", changed, err)
	}
	if err := s.DeleteWorkflowDefinition(ctx, "p", "admin", "demo", yes); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SyncWorkflowDefinitions(ctx, "p", "member", map[string]string{"demo": workflowYAML}, yes); !errors.Is(err, ErrProjectSyncDenied) {
		t.Fatalf("member recreation: %v", err)
	}
	if _, err := s.SyncWorkflowDefinitions(ctx, "p", "admin", map[string]string{"demo": workflowYAML}, yes); err != nil {
		t.Fatal(err)
	}
	d, err = s.GetWorkflowDefinition(ctx, "p", "demo")
	if err != nil || d.Version <= 7 {
		t.Fatalf("reused version: %+v %v", d, err)
	}
}

func TestWorkflowRevisionCounterBoundedPerProject(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedProject(t, s, "p", "project")
	if err := s.CreateUser(ctx, User{ID: "admin", Username: "admin", PasswordHash: "h", IsAdmin: true}); err != nil {
		t.Fatal(err)
	}
	yes := func(context.Context) bool { return true }
	for _, name := range []string{"alpha", "beta", "gamma"} {
		doc := strings.Replace(workflowYAML, "name: demo", "name: "+name, 1)
		if _, err := s.SyncWorkflowDefinitions(ctx, "p", "admin", map[string]string{name: doc}, yes); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteWorkflowDefinition(ctx, "p", "admin", name, yes); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM workflow_revision_counters WHERE project_id='p'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("counter rows = %d, err = %v", count, err)
	}
}

func TestWorkflowRevisionRejectsNonIntegerCounter(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedProject(t, s, "p", "project")
	if _, err := s.db.ExecContext(ctx, `PRAGMA ignore_check_constraints=ON; INSERT INTO workflow_revision_counters(project_id,version) VALUES ('p','invalid'); PRAGMA ignore_check_constraints=OFF`); err != nil {
		t.Fatal(err)
	}
	if err := s.initializeSchema(ctx); err == nil {
		t.Fatal("accepted a non-integer workflow revision counter")
	}
}

func TestWorkflowRevisionRejectsNonIntegerDefinitionVersion(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedProject(t, s, "p", "project")
	if err := s.CreateUser(ctx, User{ID: "admin", Username: "admin", PasswordHash: "h", IsAdmin: true}); err != nil {
		t.Fatal(err)
	}
	yes := func(context.Context) bool { return true }
	if _, err := s.SyncWorkflowDefinitions(ctx, "p", "admin", map[string]string{"demo": workflowYAML}, yes); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE workflow_definitions SET version=1.5 WHERE project_id='p'; UPDATE workflow_revision_counters SET version=7 WHERE project_id='p'`); err != nil {
		t.Fatal(err)
	}
	if err := s.initializeSchema(ctx); err == nil {
		t.Fatal("accepted a non-integer workflow definition version")
	}
}

func TestWorkflowVersionConcurrentSyncDelete(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedProject(t, s, "p", "project")
	if err := s.CreateUser(ctx, User{ID: "admin", Username: "admin", PasswordHash: "h", IsAdmin: true}); err != nil {
		t.Fatal(err)
	}
	yes := func(context.Context) bool { return true }
	defs := map[string]string{"demo": workflowYAML}
	if _, err := s.SyncWorkflowDefinitions(ctx, "p", "admin", defs, yes); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	go func() { <-start; _, err := s.SyncWorkflowDefinitions(ctx, "p", "admin", defs, yes); errs <- err }()
	go func() { <-start; errs <- s.DeleteWorkflowDefinition(ctx, "p", "admin", "demo", yes) }()
	close(start)
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SyncWorkflowDefinitions(ctx, "p", "admin", defs, yes); err != nil {
		t.Fatal(err)
	}
	d, err := s.GetWorkflowDefinition(ctx, "p", "demo")
	if err != nil || d.Version <= 1 {
		t.Fatalf("reused version after concurrent operations: %+v %v", d, err)
	}
}

func TestWorkflowDefinitionMigration(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedProject(t, s, "p", "preserved")
	if _, err := s.db.ExecContext(ctx, `DROP TABLE workflow_revision_counters; DROP TABLE workflow_definitions; PRAGMA user_version=11`); err != nil {
		t.Fatal(err)
	}
	if err := s.initializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetProject(ctx, "p"); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil || version != 14 {
		t.Fatalf("version %d: %v", version, err)
	}
}
