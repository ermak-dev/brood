package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// История старой интеграции остаётся данными, а не разрешением повторить эффект.
// Frozen fixtures не меняются: непустые записи добавляются в их временную копию.
func TestRetiredGitLabHistorySurvivesMigrationAndRepeatedOpen(t *testing.T) {
	for _, version := range []int{33, 34} {
		t.Run(fmt.Sprintf("schema-%d", version), func(t *testing.T) {
			path := loadSchemaFixture(t, version)
			raw := rawDatabase(t, path)
			seedRetiredGitLabHistory(t, raw, version)
			before := databaseRows(t, raw)
			mustNil(t, raw.Close())

			for reopen := range 2 {
				s, err := Open(context.Background(), path)
				mustNil(t, err)
				t.Cleanup(func() { s.Close() })
				t.Run(fmt.Sprintf("open-%d", reopen), func(t *testing.T) {
					requireRetiredHistoryRows(t, before, databaseRows(t, s.db))
				})
				requireIntact(t, s.db)
				var found int
				mustNil(t, s.db.QueryRow(`PRAGMA user_version`).Scan(&found))
				if found != schemaVersion {
					t.Fatalf("opened schema %d, want %d", found, schemaVersion)
				}
				for _, query := range []struct{ sql, reason string }{
					{`DELETE FROM gitlab_operations WHERE id='legacy-unknown'`, "GitLab history cannot be deleted"},
					{`UPDATE gitlab_operations SET state='completed',revision=revision+1 WHERE id='legacy-unknown'`, "GitLab operation identity or transition is invalid"},
					{`UPDATE gitlab_operations SET snapshot='rewritten',revision=revision+1 WHERE id='legacy-sending'`, "GitLab operation identity or transition is invalid"},
					{`DELETE FROM gitlab_grants WHERE id='legacy-grant'`, "GitLab history cannot be deleted"},
					{`DELETE FROM gitlab_approvals WHERE operation_id='legacy-unknown'`, "GitLab history cannot be deleted"},
				} {
					if _, err := s.db.Exec(query.sql); err == nil || !strings.Contains(err.Error(), query.reason) {
						t.Fatalf("historical fence for %q returned %v, want %q", query.sql, err, query.reason)
					}
				}
				if version == 34 {
					if _, err := s.db.Exec(`UPDATE gitlab_proposal_states SET state='pending',revision=revision+1 WHERE operation_id='legacy-rejected'`); err == nil || !strings.Contains(err.Error(), "GitLab proposal decision is terminal") {
						t.Fatalf("terminal proposal fence: %v", err)
					}
					if _, err := s.db.Exec(`DELETE FROM gitlab_proposal_states`); err == nil || !strings.Contains(err.Error(), "GitLab proposal history cannot be deleted") {
						t.Fatalf("proposal history fence: %v", err)
					}
				}
				requireRetiredHistoryRows(t, before, databaseRows(t, s.db))
				mustNil(t, s.Close())
			}
		})
	}
}

func requireRetiredHistoryRows(t *testing.T, before, after map[string][]string) {
	t.Helper()
	for table, want := range before {
		got, exists := after[table]
		if !exists || !reflect.DeepEqual(got, want) {
			t.Fatalf("historical table %s changed: exists=%v, got %v, want %v", table, exists, got, want)
		}
	}
	for table, rows := range after {
		if _, exists := before[table]; !exists && len(rows) != 0 {
			t.Fatalf("migration invented records in new table %s: %v", table, rows)
		}
	}
}

func seedRetiredGitLabHistory(t *testing.T, db *sql.DB, version int) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO gitlab_bindings VALUES ('p','https://git.example/group/repo.git','https://git.example',42,'main','fixture-reference',0,7);
	INSERT INTO gitlab_grants VALUES ('legacy-grant','p','s','native-s','worker',1,'u',7,1,1,1,1,1,2);`)
	mustNil(t, err)
	snapshot, err := json.Marshal(GitLabMergeSnapshot{
		Requester:       GitLabPrincipal{ProjectID: "p", SwarmID: "s", AtespaceUID: "native-s", ActorUID: "worker", Incarnation: 1},
		BindingRevision: 7, GitLabProjectID: 42, MRIID: 19,
		SourceBranch: "feature/history", SourceSHA: strings.Repeat("a", 40),
		PipelineID: 11, PipelineSHA: strings.Repeat("a", 40),
		TargetBranch: "main", TargetSHA: strings.Repeat("b", 40), ExpiresAt: 1,
		Evidence: "Сохранить исходные байты\n<script>not executable</script>",
	})
	mustNil(t, err)
	for _, state := range []string{"reserved", "sending", "completed", "refused", "unknown"} {
		revision := 3
		if state == "reserved" {
			revision = 1
		}
		if state == "sending" {
			revision = 2
		}
		_, err := db.Exec(`INSERT INTO gitlab_operations VALUES (?, 'p','s','native-s','worker',1,'propose',?,'legacy-grant',7,?,?,?,?,?,'2026-11-14T00:00:00Z','2026-11-14T00:00:00Z')`,
			"legacy-"+state, "request-"+state, `{"ProjectID":"p","Repository":"https://git.example/group/repo.git","CredentialRef":"fixture-reference","Revision":7}`, strings.Repeat("a", 64), string(snapshot), state, revision)
		mustNil(t, err)
	}
	for _, item := range []struct{ operation, state string }{
		{"legacy-reserved", "approved"}, {"legacy-completed", "claimed"}, {"legacy-unknown", "revoked"},
	} {
		revision := 2
		if item.state == "approved" {
			revision = 1
		}
		_, err := db.Exec(`INSERT INTO gitlab_approvals VALUES (?,'u','fixture-session-hash',1,?,?)`, item.operation, item.state, revision)
		mustNil(t, err)
	}
	if version == 34 {
		for _, state := range []string{"pending", "rejected", "stale"} {
			id := "legacy-" + state
			_, err := db.Exec(`INSERT INTO gitlab_operations VALUES (?, 'p','s','native-s','worker',1,'propose',?,'legacy-grant',7,'{}',?,?,'reserved',1,'2026-11-14T00:00:00Z','2026-11-14T00:00:00Z')`,
				id, "request-"+state, strings.Repeat("b", 64), string(snapshot))
			mustNil(t, err)
			revision := 2
			if state == "pending" {
				revision = 1
			}
			_, err = db.Exec(`INSERT INTO gitlab_proposal_states VALUES (?,?,?,'u','fixture-session-hash')`, id, state, revision)
			mustNil(t, err)
		}
	}
}
