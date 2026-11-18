package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
)

// FEATURE: FULL-фиксация внешнего child report и завершённого join перед cleanup (#292).
// SPEC: Receipt не результат, report без parent join не разрешает cleanup;
// seal не запускает lifecycle, не освобождает capacity и не является fresh effect grant.
// IMPLEMENTATION: Immutable digest seal после applied server join в FULL-транзакции.
// RESULT: IMPLEMENTED — pending/unknown/owner holds не превращаются в cleanup permission.
// STATUS: DONE
func settledEphemeralResult(t *testing.T) (*Store, EphemeralChildIntent, SwarmChildTask, SwarmChildJoin) {
	t.Helper()
	ctx := context.Background()
	s, intent, child, input := childDeliveryFixture(t)
	_, err := s.PrepareEphemeralChildDelivery(ctx, intent.ID, input, childAllow)
	mustNil(t, err)
	child, err = s.CompleteSwarmChild(ctx, intent.Parent, child.RequestID, child.Executor, child.Revision+1, "success", "Independent result outside the worker\nwith exact original bytes.", childAllow)
	mustNil(t, err)
	join, err := s.JoinSwarmChildren(ctx, intent.Parent, childAllow)
	mustNil(t, err)
	return s, intent, child, join
}

func TestEphemeralResultSealCommitsFullExactEvidenceWithoutLifecycleEffects(t *testing.T) {
	ctx := context.Background()
	s, intent, child, join := settledEphemeralResult(t)
	before := databaseRows(t, s.db)
	calls := 0
	auth := func(ctx context.Context, a SwarmChildAuthorization) error {
		calls++
		var mode int
		mustNil(t, s.conn(ctx).QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&mode))
		if mode != 2 || a.Action != "seal_ephemeral_result" || a.Parent != intent.Parent || a.Executor != child.Executor || a.RequestID != child.RequestID || a.Payload != intent.Payload {
			t.Fatal("result retention lost FULL writer or exact authority scope")
		}
		return nil
	}
	mustNil(t, s.SealEphemeralChildResult(ctx, intent.ID, auth))
	var id, reportHash, parentHash, outcome, parentOutcome, created string
	var revision int64
	mustNil(t, s.db.QueryRow(`SELECT child_id,child_revision,report_hash,parent_report_hash,outcome,parent_outcome,created_at FROM ephemeral_child_result_seals WHERE intent_id=?`, intent.ID).Scan(&id, &revision, &reportHash, &parentHash, &outcome, &parentOutcome, &created))
	childSum, parentSum := sha256.Sum256([]byte(child.Report)), sha256.Sum256([]byte(ChildJoinReport(join)))
	if id != child.ID || revision != child.Revision || reportHash != hex.EncodeToString(childSum[:]) || parentHash != hex.EncodeToString(parentSum[:]) || outcome != "success" || parentOutcome != "success" || created == "" {
		t.Fatal("seal did not bind the exact independent report and computed parent result")
	}
	after := databaseRows(t, s.db)
	delete(before, "ephemeral_child_result_seals")
	withoutSeal := databaseRows(t, s.db)
	delete(withoutSeal, "ephemeral_child_result_seals")
	if !reflect.DeepEqual(before, withoutSeal) {
		t.Fatal("retaining evidence mutated lifecycle, allocation, reports or delivery")
	}
	mustNil(t, s.SealEphemeralChildResult(ctx, intent.ID, auth))
	if calls != 2 || !reflect.DeepEqual(after, databaseRows(t, s.db)) {
		t.Fatal("seal retry skipped authority or rewrote evidence")
	}
}

func TestEphemeralResultSealRetainsFailureButNotAbsentReports(t *testing.T) {
	for _, test := range []struct{ name, report string }{{"failure", "Retained independent failure evidence"}, {"empty", ""}, {"whitespace", " \n\t"}} {
		t.Run(test.name, func(t *testing.T) {
			report := test.report
			ctx := context.Background()
			s, intent, child, input := childDeliveryFixture(t)
			_, err := s.PrepareEphemeralChildDelivery(ctx, intent.ID, input, childAllow)
			mustNil(t, err)
			_, err = s.CompleteSwarmChild(ctx, intent.Parent, child.RequestID, child.Executor, child.Revision+1, "fail", report, childAllow)
			mustNil(t, err)
			_, err = s.JoinSwarmChildren(ctx, intent.Parent, childAllow)
			mustNil(t, err)
			before := databaseRows(t, s.db)
			err = s.SealEphemeralChildResult(ctx, intent.ID, childAllow)
			if strings.TrimSpace(report) == "" {
				if err == nil || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
					t.Fatal("absent independent report obtained a result seal")
				}
				return
			}
			mustNil(t, err)
			seal, err := s.GetEphemeralChildResultSeal(ctx, intent.ID)
			mustNil(t, err)
			if seal.Outcome != "fail" || seal.ParentOutcome != "fail" {
				t.Fatal("retained failure was fabricated as successful work")
			}
		})
	}
}

func TestEphemeralResultSealSurvivesDatabaseReopen(t *testing.T) {
	ctx := context.Background()
	s, intent, _, _ := settledEphemeralResult(t)
	mustNil(t, s.SealEphemeralChildResult(ctx, intent.ID, childAllow))
	old, err := s.GetEphemeralChildResultSeal(ctx, intent.ID)
	mustNil(t, err)
	var path string
	mustNil(t, s.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path))
	mustNil(t, s.Close())
	reopened, err := Open(ctx, path)
	mustNil(t, err)
	defer reopened.Close()
	history, err := reopened.GetEphemeralChildResultSeal(ctx, intent.ID)
	mustNil(t, err)
	if history != old {
		t.Fatal("database reopen replaced externally retained result identity")
	}
	before := databaseRows(t, reopened.db)
	mustNil(t, reopened.CheckEphemeralChildResultSeal(ctx, intent.ID, childAllow))
	mustNil(t, reopened.SealEphemeralChildResult(ctx, intent.ID, childAllow))
	if !reflect.DeepEqual(before, databaseRows(t, reopened.db)) {
		t.Fatal("reopen/check/retry changed evidence or granted a new lifecycle effect")
	}
}

func TestEphemeralResultSealRefusesReportBeforeParentJoin(t *testing.T) {
	ctx := context.Background()
	s, intent, child, input := childDeliveryFixture(t)
	_, err := s.PrepareEphemeralChildDelivery(ctx, intent.ID, input, childAllow)
	mustNil(t, err)
	_, err = s.CompleteSwarmChild(ctx, intent.Parent, child.RequestID, child.Executor, child.Revision+1, "success", "Independent report, parent has not joined", childAllow)
	mustNil(t, err)
	before := databaseRows(t, s.db)
	if err := s.SealEphemeralChildResult(ctx, intent.ID, childAllow); err == nil {
		t.Fatal("child report without completed parent join obtained result seal")
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("refused result seal changed retained facts")
	}
}
