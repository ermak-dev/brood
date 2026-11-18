package proxmox

// FEATURE: Read-only выбор VMID до admission и отправки Create.
// SPEC: Локальные holds неприкосновенны; только exact global nil разрешает ID.
// Только collision продолжает scan; budget ограничивает confirmations, hint не permission.
// IMPLEMENTATION: Ограниченный read-only scan через VMIDRegistry, без allocation и writes.
// RESULT: IMPLEMENTED — локальные holds пропускаются, отсутствие подтверждается заново.
// STATUS: DONE

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"strconv"
	"testing"
	"time"
)

type selectionRegistry struct {
	suggestion int
	nextErr    error
	nextCalls  int
	checks     []int
	confirm    func(context.Context, int) error
}

func (r *selectionRegistry) NextID(context.Context) (int, error) {
	r.nextCalls++
	return r.suggestion, r.nextErr
}
func (r *selectionRegistry) ConfirmAbsent(ctx context.Context, id int) error {
	r.checks = append(r.checks, id)
	if r.confirm != nil {
		return r.confirm(ctx, id)
	}
	return nil
}

func TestSelectAvailableVMID(t *testing.T) {
	held := map[int]struct{}{123: {}, 124: {}, 126: {}}
	before := maps.Clone(held)
	r := &selectionRegistry{suggestion: 123, confirm: func(_ context.Context, id int) error {
		if id == 125 {
			return fmt.Errorf("occupied: %w", ErrCollision)
		}
		return nil
	}}
	got, err := SelectAvailableVMID(context.Background(), r, held, 0, 2)
	if err != nil || got.ID != 127 || got.ResumeAt != 0 {
		t.Fatalf("selection = %+v, %v", got, err)
	}
	if r.nextCalls != 1 || !reflect.DeepEqual(r.checks, []int{125, 127}) {
		t.Fatalf("reads = %d, %v", r.nextCalls, r.checks)
	}
	if !maps.Equal(held, before) {
		t.Fatal("selection changed local reservations")
	}
}

func TestSelectAvailableVMIDBudgetAndResume(t *testing.T) {
	r := &selectionRegistry{suggestion: 200, confirm: func(context.Context, int) error { return ErrCollision }}
	got, err := SelectAvailableVMID(context.Background(), r, nil, 0, 2)
	if err == nil || got.ID != 0 || got.ResumeAt != 202 || !reflect.DeepEqual(r.checks, []int{200, 201}) {
		t.Fatalf("batch = %+v, %v, checks %v", got, err, r.checks)
	}
	// Новый проход обязан снова проверить candidate, даже после предыдущего nil.
	r.confirm = nil
	held := map[int]struct{}{202: {}}
	got, err = SelectAvailableVMID(context.Background(), r, held, got.ResumeAt, 1)
	if err != nil || got.ID != 203 || r.nextCalls != 1 || !reflect.DeepEqual(r.checks, []int{200, 201, 203}) {
		t.Fatalf("resume = %+v, %v, reads %v", got, err, r.checks)
	}
	held[203] = struct{}{}
	got, err = SelectAvailableVMID(context.Background(), r, held, 203, 1)
	if err != nil || got.ID != 204 {
		t.Fatalf("hint bypassed hold: %+v %v", got, err)
	}
}

func TestSelectAvailableVMIDNoHiddenCap(t *testing.T) {
	r := &selectionRegistry{suggestion: 100, confirm: func(_ context.Context, id int) error {
		if id < 1124 {
			return ErrCollision
		}
		return nil
	}}
	got, err := SelectAvailableVMID(context.Background(), r, nil, 0, 1025)
	if err != nil || got.ID != 1124 || len(r.checks) != 1025 {
		t.Fatalf("selection = %+v, %v, checks %d", got, err, len(r.checks))
	}
}

func TestSelectAvailableVMIDStopsOnErrors(t *testing.T) {
	for _, failure := range []error{ErrPermission, errors.New("transport unavailable"), errors.New("malformed response"), context.Canceled, context.DeadlineExceeded} {
		t.Run(failure.Error(), func(t *testing.T) {
			r := &selectionRegistry{suggestion: 123, confirm: func(context.Context, int) error { return failure }}
			got, err := SelectAvailableVMID(context.Background(), r, nil, 0, 10)
			if !errors.Is(err, failure) || got.ID != 0 || got.ResumeAt != 0 || len(r.checks) != 1 {
				t.Fatalf("failure = %+v %v, checks %v", got, err, r.checks)
			}
			r = &selectionRegistry{nextErr: failure}
			got, err = SelectAvailableVMID(context.Background(), r, nil, 0, 10)
			if !errors.Is(err, failure) || got.ID != 0 || len(r.checks) != 0 {
				t.Fatalf("suggestion failure = %+v %v", got, err)
			}
		})
	}
	// Collision у suggestion не является отказом exact candidate.
	r := &selectionRegistry{suggestion: 123, nextErr: ErrCollision}
	got, err := SelectAvailableVMID(context.Background(), r, nil, 0, 5)
	if !errors.Is(err, ErrCollision) || got.ID != 0 || len(r.checks) != 0 {
		t.Fatalf("suggestion collision = %+v %v", got, err)
	}
}

func TestSelectAvailableVMIDValidationAndRange(t *testing.T) {
	for _, hint := range []int{-1, 1, 99, 1000000000, int(^uint(0) >> 1)} {
		r := &selectionRegistry{suggestion: 123}
		got, err := SelectAvailableVMID(context.Background(), r, nil, hint, 1)
		if err == nil || got != (VMIDSelection{}) || r.nextCalls != 0 || len(r.checks) != 0 {
			t.Fatalf("invalid hint %d = %+v %v", hint, got, err)
		}
	}
	for _, id := range []int{-1, 0, 99, 1000000000} {
		r := &selectionRegistry{suggestion: id}
		got, err := SelectAvailableVMID(context.Background(), r, nil, 0, 1)
		if err == nil || got.ID != 0 || len(r.checks) != 0 {
			t.Fatalf("invalid suggestion %d = %+v %v", id, got, err)
		}
	}
	for _, budget := range []int{-1, 0} {
		r := &selectionRegistry{suggestion: 123}
		got, err := SelectAvailableVMID(context.Background(), r, nil, 0, budget)
		if err == nil || got.ID != 0 || r.nextCalls != 0 || len(r.checks) != 0 {
			t.Fatalf("invalid budget = %+v %v", got, err)
		}
	}
	got, err := SelectAvailableVMID(context.Background(), nil, nil, 0, 1)
	if err == nil || got.ID != 0 {
		t.Fatalf("nil registry = %+v %v", got, err)
	}
	for _, id := range []int{100, 999999999} {
		r := &selectionRegistry{suggestion: id}
		got, err := SelectAvailableVMID(context.Background(), r, nil, 0, 1)
		if err != nil || got.ID != id {
			t.Fatalf("boundary %d = %+v %v", id, got, err)
		}
	}
	for _, held := range []bool{false, true} {
		r := &selectionRegistry{suggestion: 999999999, confirm: func(context.Context, int) error { return ErrCollision }}
		reserved := map[int]struct{}{}
		if held {
			reserved[999999999] = struct{}{}
		}
		got, err := SelectAvailableVMID(context.Background(), r, reserved, 0, 1)
		if err == nil || got != (VMIDSelection{}) || len(r.checks) != map[bool]int{true: 0, false: 1}[held] {
			t.Fatalf("range exhaustion = %+v %v checks %v", got, err, r.checks)
		}
	}
}

func TestSelectAvailableVMIDCancellation(t *testing.T) {
	for _, stage := range []string{"before", "deadline", "suggestion", "confirmation", "collision"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := context.Canceled
			r := &selectionRegistry{suggestion: 123}
			var registry VMIDRegistry = r
			switch stage {
			case "before":
				cancel()
			case "deadline":
				var c context.CancelFunc
				ctx, c = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				defer c()
				want = context.DeadlineExceeded
			case "suggestion":
				registry = &cancelSuggestion{r: r, cancel: cancel}
			default:
				r.confirm = func(context.Context, int) error {
					cancel()
					if stage == "collision" {
						return ErrCollision
					}
					return nil
				}
			}
			got, err := SelectAvailableVMID(ctx, registry, nil, 0, 3)
			if !errors.Is(err, want) || got.ID != 0 {
				t.Fatalf("cancel = %+v %v", got, err)
			}
			if (stage == "before" || stage == "deadline") && r.nextCalls != 0 {
				t.Fatal("called registry after cancellation")
			}
			if (stage == "suggestion") && len(r.checks) != 0 {
				t.Fatal("confirmed after canceled suggestion")
			}
			if len(r.checks) > 1 {
				t.Fatal("continued after cancellation")
			}
		})
	}
}

type cancelSuggestion struct {
	r      *selectionRegistry
	cancel context.CancelFunc
}

func (r *cancelSuggestion) NextID(ctx context.Context) (int, error) {
	r.cancel()
	return r.r.NextID(ctx)
}
func (r *cancelSuggestion) ConfirmAbsent(ctx context.Context, id int) error {
	return r.r.ConfirmAbsent(ctx, id)
}

func TestSelectAvailableVMIDTLSGlobalEvidenceOnly(t *testing.T) {
	for _, scenario := range []string{"free", "collision-400", "collision-409", "permission", "outage", "malformed", "wrong-id", "bad-suggestion"} {
		t.Run(scenario, func(t *testing.T) {
			var requests []string
			c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				requests = append(requests, r.Method+" "+r.URL.RequestURI())
				if r.Method != "GET" || r.URL.Path != "/api2/json/cluster/nextid" {
					t.Errorf("non-global read or write: %s %s", r.Method, r.URL)
				}
				if r.URL.RawQuery == "" {
					if scenario == "bad-suggestion" {
						envelope(w, "00123")
					} else {
						envelope(w, 123)
					}
					return
				}
				id, e := strconv.Atoi(r.URL.Query().Get("vmid"))
				if e != nil {
					t.Error(e)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if id == 124 {
					envelope(w, 124)
					return
				}
				switch scenario {
				case "free":
					envelope(w, 123)
				case "collision-400":
					w.WriteHeader(400)
				case "collision-409":
					w.WriteHeader(409)
				case "permission":
					w.WriteHeader(403)
				case "outage":
					w.WriteHeader(503)
				case "malformed":
					envelope(w, map[string]any{"vmid": 123})
				default:
					envelope(w, 124)
				}
			})
			got, err := SelectAvailableVMID(context.Background(), c, nil, 0, 2)
			want := 0
			switch scenario {
			case "free":
				want = 123
			case "collision-400", "collision-409":
				want = 124
			}
			if got.ID != want || (err == nil) != (want != 0) {
				t.Fatalf("selection = %+v %v", got, err)
			}
			n := 2
			if want == 124 {
				n = 3
			}
			if scenario == "bad-suggestion" {
				n = 1
			}
			if len(requests) != n {
				t.Fatalf("unexpected retries/effects: %v", requests)
			}
			noLeak(t, got, err)
		})
	}
}
