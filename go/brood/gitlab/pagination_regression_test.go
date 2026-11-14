package gitlab

// BUG: Oversized arrays allocate every DTO; Link spelling variants omit failures.
// HYPOTHESIS: The row cap follows full Unmarshal and rel matching is case-sensitive.
// SOLUTION: Decode only 100 array entries; normalize Link casing/whitespace and
// refuse unsupported or ambiguous pagination rather than returning a prefix.
// RESULT: FIXED - Allocation is bounded; row 101 is untouched; page 2 fails CI.
// STATUS: FIXED

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"testing"
)

func TestLinkPaginationIncludesFailedSecondPage(t *testing.T) {
	for _, relation := range []string{`Rel="next"`, `rel="NEXT"`, `rel = "next"`, "REL\t=\tNeXt", `rel="last NEXT"`, `rel="last  NEXT"`, `rel="prev NEXT"`, `rel="prev  NEXT"`} {
		t.Run(relation, func(t *testing.T) {
			calls := 0
			firstCount := 1
			if strings.Contains(relation, "last") {
				// Объявленный last требует полной нетерминальной страницы.
				firstCount = pageSize
			}
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if strings.HasSuffix(r.URL.Path, "/trigger_jobs") {
					fmt.Fprint(w, `[]`)
					return
				}
				if r.URL.Query().Get("page") != fmt.Sprint(calls) {
					t.Error("request did not use the next local numeric page")
				}
				if calls == 1 {
					w.Header().Set("Link", "<https://"+r.Host+r.URL.Path+"?page=2&per_page=100&include_retried=false>; "+relation)
				}
				count, start, status := firstCount, 1, "success"
				if calls == 2 {
					count, start, status = 1, firstCount+1, "failed"
				}
				rows := make([]Job, count)
				for i := range rows {
					rows[i] = Job{ID: int64(start + i), Status: status, Pipeline: Pipeline{ID: 90, ProjectID: 42, SHA: "abc"}}
				}
				_ = json.NewEncoder(w).Encode(rows)
			})
			jobs, err := c.Jobs(context.Background(), 90)
			if strings.Contains(relation, "prev") {
				// Both tokens apply to one target: page 2 cannot be prev on page 1.
				checkError(t, err, Response)
				if calls != 1 || jobs != nil {
					t.Fatal("contradictory relations accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			evidence := AssessCI(Pipeline{ID: 90, ProjectID: 42, SHA: "abc", Status: "success"}, jobs)
			if calls != 3 || len(jobs) != firstCount+1 || evidence.Verdict != CIFailed || len(evidence.FailedJobIDs) != 1 || evidence.FailedJobIDs[0] != int64(firstCount+1) {
				t.Fatalf("silently incomplete evidence: requests=%d jobs=%d evidence=%+v", calls, len(jobs), evidence)
			}
		})
	}
}

func TestPageDecodingAllocationBound(t *testing.T) {
	const count = 100000
	body := "[" + strings.Repeat("{},", count-1) + "{}]"
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	jobs, err := c.Jobs(context.Background(), 90)
	runtime.ReadMemStats(&after)
	checkError(t, err, Response)
	if jobs != nil {
		t.Fatal("oversized page returned a prefix")
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("response=%d bytes objects=%d heap_total_allocated=%d bytes error=%v", len(body), count, allocated, err)
	// Includes the real TLS transport and bounded body read, not just a decoder
	// microbenchmark. Full slice decoding allocates about 123 MB for this body.
	if allocated > 16<<20 {
		t.Fatalf("row limit enforced only after allocating %d bytes", allocated)
	}
}

type forbiddenPageTail struct{}

func (forbiddenPageTail) UnmarshalJSON([]byte) error {
	panic("decoded entry beyond the 100-row page bound")
}

func TestPageDecodingDoesNotDecodeExtraRow(t *testing.T) {
	defer func() {
		if v := recover(); v != nil {
			t.Fatalf("page decoder visited a forbidden tail: %v", v)
		}
	}()
	type row struct {
		ID   int64             `json:"id"`
		Tail forbiddenPageTail `json:"tail"`
	}
	var body strings.Builder
	body.WriteByte('[')
	for i := 1; i <= pageSize; i++ {
		fmt.Fprintf(&body, `{"id":%d},`, i)
	}
	body.WriteString(`{"id":101,"tail":true}]`)
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body.String()) })
	remaining, pages := MaxOperationBytes, MaxPages
	rows, err := readPages[row](c, context.Background(), &remaining, &pages, "/pipelines/90/jobs", nil, func(r row) int64 { return r.ID })
	checkError(t, err, Response)
	if rows != nil {
		t.Fatal("oversized page returned a prefix")
	}
}

func TestPaginationRejectsUnsupportedLink(t *testing.T) {
	for _, relation := range []string{
		``, `rel`, `rel=`, `rel=""`, `rel="next`, `rel=next"`,
		`rel="next"; rel="prev"`, `rel="prev"; rel="next"`,
		`rel="next"; title="a,b;c"`, `title="next"`, `rel="unknown"`,
		`rel="next\\"`, `rel="next";`,
	} {
		t.Run(relation, func(t *testing.T) {
			calls := 0
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Link", "<https://"+r.Host+r.URL.Path+"?page=2&per_page=100&include_retried=false>; "+relation)
				fmt.Fprint(w, `[{"id":1,"status":"success","pipeline":{"id":90,"project_id":42,"sha":"abc"}}]`)
			})
			jobs, err := c.Jobs(context.Background(), 90)
			checkError(t, err, Response)
			if calls != 1 || jobs != nil {
				t.Fatal("unsupported Link returned partial evidence or made another request")
			}
		})
	}
}

func TestPageDecodingRejectsInvalidShapesAndTrailingData(t *testing.T) {
	for _, body := range []string{
		`null`, `{}`, `[null]`, `[[]]`, `["x"]`, `[] []`, `[] trailing`,
		`[{"id":1,"pipeline":{"id":90,"project_id":42}}`,
		`[{"id":1,"pipeline":{"id":90,"project_id":42}},]`,
	} {
		t.Run(body, func(t *testing.T) {
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
			jobs, err := c.Jobs(context.Background(), 90)
			checkError(t, err, Response)
			if jobs != nil {
				t.Fatal("invalid page returned a prefix")
			}
		})
	}
}
