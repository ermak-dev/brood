package gitlab

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// FEATURE: Bounded create input also fits a durable canonical intent payload.
// SPEC: The existing journal caps the entire canonical payload at 64 KiB, not
// just description text. Leave room even with JSON's six-byte HTML escaping.
// IMPLEMENTATION: The eight-KiB text cap leaves headroom for the other fields
// and correlation marker without importing or mutating the store.
// RESULT: IMPLEMENTED - Worst-case escaped fields fit the existing intent budget.
// STATUS: DONE
func TestCreateCanonicalIntentBudget(t *testing.T) {
	in := createInput()
	in.SourceBranch = strings.Repeat("s", MaxCreateBranchBytes)
	in.TargetBranch = strings.Repeat("t", MaxCreateBranchBytes)
	in.SourceSHA = strings.Repeat("a", 64)
	in.Title = strings.Repeat("&", MaxCreateTitleBytes)
	in.Description = strings.Repeat("<", MaxCreateDescriptionBytes)
	in.RequestID = strings.Repeat("r", MaxCreateRequestIDBytes)
	canonical, err := json.Marshal(in)
	if err != nil || len(canonical) > 65536 {
		t.Fatalf("bounded arguments cannot fit a canonical intent: bytes=%d err=%v", len(canonical), err)
	}
}

func TestCreateDescriptionReservesIntentHeadroom(t *testing.T) {
	var calls atomic.Int32
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); writeCreated(w, createdMR()) })
	in := createInput()
	in.Description = strings.Repeat("d", 8193)
	_, err := c.CreateMergeRequest(context.Background(), "main", in)
	checkCreateError(t, err, Invalid, CreateNotSent, 0)
	if calls.Load() != 0 {
		t.Fatal("oversized canonical input sent")
	}
}
