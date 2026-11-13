package store

// FEATURE: Unsent stage marks of managed actors (#277).
// SPEC: A stop or delete stage whose PVE write has not been requested carries
// an unsent mark at the start of Error, and the public diagnostic follows it.
// Only the stage transitions that create such a stage set the mark
// (BeginManagedActorOperation, ConfirmManagedActorStage,
// MarkManagedActorDeleteUnsent); UpdateManagedActor keeps or drops a mark but
// never sets one, so an observation's diagnostic text cannot forge a mark.
// The store cannot see whether a write reached PVE: MarkManagedActorDeleteUnsent
// trusts its caller's proof that the Delete failed before any write and only
// refuses a bound task or another stage. A delete stage without the mark
// stays possibly submitted: only that caller-proved transition gives it the
// mark again, and it does not return to its stop stage. A tombstone keeps
// no mark, and stage transitions are fenced by revision and operation ID.
// IMPLEMENTATION: Store-level parser and fenced stage transitions.
// RESULT: IMPLEMENTED
// STATUS: DONE

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestManagedUnsentMarkSplitsDiagnostic(t *testing.T) {
	for text, want := range map[string][2]string{
		"":                  {"", ""},
		ManagedStopUnsent:   {ManagedStopUnsent, ""},
		ManagedDeleteUnsent: {ManagedDeleteUnsent, ""},
		ManagedStopUnsent + ": Proxmox permission denied; allocation retained": {ManagedStopUnsent, "Proxmox permission denied; allocation retained"},
		ManagedDeleteUnsent + ": Proxmox reconciliation unavailable":           {ManagedDeleteUnsent, "Proxmox reconciliation unavailable"},
		"Proxmox permission denied; allocation retained":                       {"", "Proxmox permission denied; allocation retained"},
		ManagedStopUnsent + "ed":                                               {"", ManagedStopUnsent + "ed"},
		"last " + ManagedDeleteUnsent:                                          {"", "last " + ManagedDeleteUnsent},
	} {
		a := ManagedActor{Error: text}
		if got := [2]string{a.Unsent(), a.Diagnostic()}; got != want {
			t.Errorf("%q: mark and diagnostic %q, want %q", text, got, want)
		}
		if want[0] != "" {
			if joined := ManagedUnsentError(want[0], want[1]); joined != text {
				t.Errorf("join of %q: %q", want, joined)
			}
		}
	}
	if got := ManagedUnsentError("", "diagnostic"); got != "diagnostic" {
		t.Errorf("join without a mark: %q", got)
	}
}

// unsentRunning — актор a, чья CT работает после согласованного start.
func unsentRunning(t *testing.T, s *Store, uid string, ctid int) ManagedActor {
	t.Helper()
	a, err := s.ReserveManagedActor(context.Background(), managedRequest(uid, "one", ctid))
	if err != nil {
		t.Fatal(err)
	}
	a = lifecycleConfirm(t, s, a, "stopped")
	a = lifecycleBegin(t, s, a, "start")
	return lifecycleConfirm(t, s, a, "running")
}

// unsentDelete — стадия delete операции operation, чей Delete не запрошен.
func unsentDelete(t *testing.T, s *Store, operation string) ManagedActor {
	t.Helper()
	a := lifecycleBegin(t, s, unsentRunning(t, s, "a", 101), operation)
	a = lifecycleConfirm(t, s, a, "stopped")
	if a.Stage != "delete" || a.UPID != "" || a.Phase != "stopped" || a.Unsent() != ManagedDeleteUnsent || a.Diagnostic() != "" {
		t.Fatalf("unsent %s delete stage: %+v", operation, a)
	}
	return a
}

func TestManagedUnsentMarksOfStages(t *testing.T) {
	for _, operation := range []string{"stop", "delete", "reset", "update"} {
		t.Run(operation, func(t *testing.T) {
			s := newTestStore(t)
			managedFixture(t, s)
			a := lifecycleBegin(t, s, unsentRunning(t, s, "a", 101), operation)
			if a.Stage != "stop" || a.UPID != "" || a.Unsent() != ManagedStopUnsent || a.Diagnostic() != "" {
				t.Fatalf("%s stop stage: %+v", operation, a)
			}
			a = lifecycleConfirm(t, s, a, "stopped")
			want := ""
			if operation == "delete" || operation == "reset" {
				want = ManagedDeleteUnsent
			}
			if a.Unsent() != want || a.Diagnostic() != "" {
				t.Fatalf("%s after the confirmed stop: %+v", operation, a)
			}
		})
	}
	t.Run("start", func(t *testing.T) {
		s := newTestStore(t)
		managedFixture(t, s)
		a := lifecycleConfirm(t, s, lifecycleBegin(t, s, unsentRunning(t, s, "a", 101), "stop"), "stopped")
		if a = lifecycleBegin(t, s, a, "start"); a.Error != "" {
			t.Fatalf("start stage: %+v", a)
		}
	})
	// Отсутствующая CT кончает очистку без Delete: отметки нет.
	t.Run("absent", func(t *testing.T) {
		s := newTestStore(t)
		managedFixture(t, s)
		a := lifecycleConfirm(t, s, lifecycleBegin(t, s, unsentRunning(t, s, "a", 101), "delete"), "absent")
		if a.Stage != "delete" || a.Error != "" {
			t.Fatalf("absent cleanup: %+v", a)
		}
	})
}

func TestManagedUnsentMarkIsNotSetByObservation(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	managedFixture(t, s)
	a := unsentDelete(t, s, "delete")
	// Отметку сохраняет проход, сменивший диагностику.
	a.Error = ManagedUnsentError(ManagedDeleteUnsent, "Proxmox permission denied; allocation retained")
	a, err := s.UpdateManagedActor(ctx, a, a.Revision)
	if err != nil || a.Unsent() != ManagedDeleteUnsent {
		t.Fatalf("kept mark: %+v %v", a, err)
	}
	// Отметка снята перед отправкой Delete: запись могла уйти.
	a.Phase, a.Error = "deleting", ""
	if a, err = s.UpdateManagedActor(ctx, a, a.Revision); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{ManagedDeleteUnsent, ManagedUnsentError(ManagedDeleteUnsent, "Proxmox reconciliation unavailable; allocation retained"), ManagedStopUnsent} {
		forged := a
		forged.Phase, forged.Error = "stopped", text
		if _, err := s.UpdateManagedActor(ctx, forged, a.Revision); err == nil {
			t.Errorf("observation set the unsent mark %q", text)
		}
	}
	got, err := s.GetManagedActor(ctx, a.UID)
	if err != nil || !reflect.DeepEqual(got, a) {
		t.Fatalf("refused observation changed the row: %+v %v", got, err)
	}
	if _, err := s.ReopenManagedActorStop(ctx, a.UID, a.Revision, a.OperationID); err == nil {
		t.Fatal("a possibly submitted delete returned to its stop stage")
	}
	// Отказ Delete до записи возвращает отметку явным переходом.
	a, err = s.MarkManagedActorDeleteUnsent(ctx, a.UID, a.Revision, a.OperationID, "Proxmox reconciliation unavailable; allocation retained")
	if err != nil || a.Stage != "delete" || a.Phase != "stopped" || a.UPID != "" || a.Unsent() != ManagedDeleteUnsent || a.Diagnostic() != "Proxmox reconciliation unavailable; allocation retained" {
		t.Fatalf("unsent delete: %+v %v", a, err)
	}
	bound := lifecycleBind(t, s, a, "UPID:delete")
	if _, err := s.MarkManagedActorDeleteUnsent(ctx, bound.UID, bound.Revision, bound.OperationID, ""); err == nil {
		t.Fatal("a bound delete task was marked unsent")
	}
	if _, err := s.ReopenManagedActorStop(ctx, bound.UID, bound.Revision, bound.OperationID); err == nil {
		t.Fatal("a bound delete task returned to its stop stage")
	}
}

// TestManagedUnsentDeleteReopensStop: CT, чей Delete не запрошен, запустили
// мимо brood. Стадия возвращается к незапрошенной остановке той же операции.
func TestManagedUnsentDeleteReopensStop(t *testing.T) {
	for _, operation := range []string{"delete", "reset"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			s := newTestStore(t)
			managedFixture(t, s)
			a := unsentDelete(t, s, operation)
			b, err := s.ReopenManagedActorStop(ctx, a.UID, a.Revision, a.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			want := a
			want.Stage, want.Phase, want.Error, want.Revision, want.UpdatedAt = "stop", "running", "", a.Revision+1, b.UpdatedAt
			if !reflect.DeepEqual(b, want) {
				t.Fatalf("reopened stop: %+v want %+v", b, want)
			}
			if _, err := s.ReopenManagedActorStop(ctx, b.UID, b.Revision, b.OperationID); err == nil {
				t.Fatal("a stop stage was reopened")
			}
		})
	}
}

// TestManagedDeletedTombstoneHasNoUnsentMark: CT ждущего Delete нашлась
// отсутствующей, и удаление подтверждено без Delete. Удалённая строка — не
// ждущая стадия: отметки на ней нет, а диагностика последнего отказа
// остаётся, как у любой строки, удалённой после отказа прохода.
func TestManagedDeletedTombstoneHasNoUnsentMark(t *testing.T) {
	for name, diagnostic := range map[string]string{"bare mark": "", "mark and diagnostic": "Proxmox reconciliation unavailable; allocation retained"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := newTestStore(t)
			managedFixture(t, s)
			a := unsentDelete(t, s, "delete")
			if diagnostic != "" {
				a.Error = ManagedUnsentError(ManagedDeleteUnsent, diagnostic)
				var err error
				if a, err = s.UpdateManagedActor(ctx, a, a.Revision); err != nil {
					t.Fatal(err)
				}
			}
			b, err := s.ConfirmManagedActorDeleted(ctx, a.UID, a.Revision, a.OperationID)
			if err != nil || b.Phase != "deleted" || b.Stage != "done" || b.Unsent() != "" || b.Error != diagnostic {
				t.Fatalf("tombstone of an unsent delete: %+v %v", b, err)
			}
			if got, err := s.GetManagedActor(ctx, a.UID); err != nil || !reflect.DeepEqual(got, b) {
				t.Fatalf("stored tombstone: %+v %v, want %+v", got, err, b)
			}
		})
	}
}

// TestManagedReopenStopIsFenced: возврат к остановке — переход стадии,
// огороженный ревизией и операцией: наблюдение, прочитанное до чужой записи
// или другой операцией, стадию не меняет.
func TestManagedReopenStopIsFenced(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	managedFixture(t, s)
	stale := unsentDelete(t, s, "delete")
	// Проход сменил диагностику после того, как вызывающий прочитал строку.
	newer := stale
	newer.Error = ManagedUnsentError(ManagedDeleteUnsent, "Proxmox reconciliation unavailable; allocation retained")
	newer, err := s.UpdateManagedActor(ctx, newer, stale.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReopenManagedActorStop(ctx, stale.UID, stale.Revision, stale.OperationID); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale reopen: %v, want %v", err, ErrRevisionConflict)
	}
	if _, err := s.ReopenManagedActorStop(ctx, newer.UID, newer.Revision, "another-operation"); err == nil {
		t.Fatal("another operation reopened the stop stage")
	}
	if got, err := s.GetManagedActor(ctx, newer.UID); err != nil || !reflect.DeepEqual(got, newer) {
		t.Fatalf("refused reopen changed the row: %+v %v, want %+v", got, err, newer)
	}
	if b, err := s.ReopenManagedActorStop(ctx, newer.UID, newer.Revision, newer.OperationID); err != nil || b.Stage != "stop" || b.Phase != "running" || b.Error != "" {
		t.Fatalf("fresh reopen: %+v %v", b, err)
	}
}
