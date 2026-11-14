package gitlab

// BUG: Completeness must cover bridge bindings, late failures and zero totals.
// HYPOTHESIS: Independent endpoint reads can reset limits or discard metadata.
// SOLUTION: Retain pagination constraints and accept validated terminal pages;
// trigger failures discard build prefixes and fallback consumes the same budget.
// RESULT: FIXED - Zero totals, late 404s and bridge binding errors fail closed.
// STATUS: FIXED

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestPaginationTotalsMustMatchRows(t *testing.T) {
	for _, header := range []string{"X-Total", "X-Total-Pages"} {
		t.Run(header, func(t *testing.T) {
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/trigger_jobs") {
					fmt.Fprint(w, `[]`)
					return
				}
				w.Header().Set(header, "0")
				fmt.Fprint(w, `[{"id":1,"status":"success","pipeline":{"id":90,"project_id":42,"sha":"abc"}}]`)
			})
			jobs, err := c.Jobs(context.Background(), 90)
			checkError(t, err, Response)
			if jobs != nil {
				t.Fatal("zero total returned nonempty CI evidence")
			}
		})
	}
}

func TestFullTerminalPageStillReadsTriggers(t *testing.T) {
	calls := 0
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if strings.HasSuffix(r.URL.Path, "/trigger_jobs") {
			fmt.Fprint(w, `[{"id":101,"status":"failed","pipeline":{"id":90,"project_id":42,"sha":"abc"}}]`)
			return
		}
		if r.URL.Query().Get("page") != "1" {
			fmt.Fprint(w, `[]`)
			return
		}
		w.Header().Set("Link", "<https://"+r.Host+r.URL.Path+"?page=1&per_page=100&include_retried=false>; rel=\"first last\"")
		w.Header().Set("X-Total-Pages", "1")
		w.Header().Set("X-Total", "100")
		rows := make([]Job, 100)
		for i := range rows {
			rows[i] = Job{ID: int64(i + 1), Status: "success", Pipeline: Pipeline{ID: 90, ProjectID: 42, SHA: "abc"}}
		}
		_ = json.NewEncoder(w).Encode(rows)
	})
	jobs, err := c.Jobs(context.Background(), 90)
	if err != nil {
		t.Fatal(err)
	}
	e := AssessCI(Pipeline{ID: 90, ProjectID: 42, SHA: "abc", Status: "success"}, jobs)
	if calls != 2 || len(jobs) != 101 || e.Verdict != CIFailed || len(e.FailedJobIDs) != 1 || e.FailedJobIDs[0] != 101 {
		t.Fatalf("terminal metadata lost or triggers skipped: calls=%d rows=%d evidence=%+v", calls, len(jobs), e)
	}
}

func TestEarlierLastPageCannotBeForgotten(t *testing.T) {
	calls := 0
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		count := 1
		if page == 1 {
			base := "https://" + r.Host + r.URL.Path + "?per_page=100&include_retried=false&page="
			w.Header().Set("Link", "<"+base+"2>; rel=next, <"+base+"3>; rel=last")
			count = pageSize
		}
		rows := make([]Job, count)
		for i := range rows {
			rows[i] = Job{ID: int64((page-1)*pageSize + i + 1), Pipeline: Pipeline{ID: 90, ProjectID: 42}}
		}
		_ = json.NewEncoder(w).Encode(rows)
	})
	jobs, err := c.Jobs(context.Background(), 90)
	checkError(t, err, Response)
	if jobs != nil || calls != 2 {
		t.Fatalf("forgot earlier last-page evidence or refused before reaching it: calls=%d rows=%d", calls, len(jobs))
	}
}

func TestTriggerBindingsAndMalformedResponses(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		kind       ErrorKind
	}{
		{"foreign project", `[{"id":2,"pipeline":{"id":90,"project_id":43}}]`, Binding},
		{"foreign pipeline", `[{"id":2,"pipeline":{"id":91,"project_id":42}}]`, Binding},
		{"missing binding", `[{"id":2}]`, Binding},
		{"duplicate build id", `[{"id":1,"pipeline":{"id":90,"project_id":42}}]`, Response},
		{"duplicate bridge id", `[{"id":2,"pipeline":{"id":90,"project_id":42}},{"id":2,"pipeline":{"id":90,"project_id":42}}]`, Response},
		{"null", `null`, Response},
		{"truncated", `[{"id":2`, Response},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/jobs") {
					fmt.Fprint(w, `[{"id":1,"pipeline":{"id":90,"project_id":42}}]`)
					return
				}
				fmt.Fprint(w, tt.body)
			})
			jobs, err := c.Jobs(context.Background(), 90)
			checkError(t, err, tt.kind)
			if jobs != nil {
				t.Fatal("unverified bridge or build prefix returned")
			}
		})
	}
}

func TestTriggerLate404DoesNotFallBack(t *testing.T) {
	calls := 0
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch {
		case strings.HasSuffix(r.URL.Path, "/jobs"):
			fmt.Fprint(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/trigger_jobs") && r.URL.Query().Get("page") == "1":
			w.Header().Set("X-Next-Page", "2")
			fmt.Fprint(w, `[{"id":2,"pipeline":{"id":90,"project_id":42}}]`)
		case strings.HasSuffix(r.URL.Path, "/trigger_jobs"):
			http.NotFound(w, r)
		default:
			t.Error("late failure incorrectly used compatibility fallback")
			fmt.Fprint(w, `[]`)
		}
	})
	jobs, err := c.Jobs(context.Background(), 90)
	checkError(t, err, HTTP)
	if calls != 3 || jobs != nil {
		t.Fatalf("late error hidden: calls=%d jobs=%v", calls, jobs)
	}
}

func TestTriggerFallbackConsumesSharedPageBudget(t *testing.T) {
	calls := 0
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if strings.HasSuffix(r.URL.Path, "/trigger_jobs") {
			http.NotFound(w, r)
			return
		}
		build := strings.HasSuffix(r.URL.Path, "/jobs")
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		last := 2
		if build {
			last = 8
		}
		if page < last {
			w.Header().Set("X-Next-Page", strconv.Itoa(page+1))
		}
		id := page
		if !build {
			id += 100
		}
		fmt.Fprintf(w, `[{"id":%d,"pipeline":{"id":90,"project_id":42}}]`, id)
	})
	jobs, err := c.Jobs(context.Background(), 90)
	checkError(t, err, Limit)
	if calls != MaxPages || jobs != nil {
		t.Fatalf("compatibility probe reset budget: calls=%d jobs=%v", calls, jobs)
	}
}
