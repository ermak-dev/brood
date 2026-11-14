package gitlab

// BUG: Ignored non-next links and omitted trigger jobs permit false CI success.
// HYPOTHESIS: Pagination validates only next, and Jobs reads only build jobs.
// SOLUTION: Validate every relation/header, retain totals and combine typed
// trigger reads with builds under one page/body/deadline budget; narrow 404 fallback.
// RESULT: FIXED - Last-only evidence refuses; failed bridges cannot disappear.
// STATUS: FIXED

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAllPaginationRelationsFailClosed(t *testing.T) {
	current, _ := url.Parse("https://git.example/api/v4/projects/42/pipelines/90/jobs?page=1&per_page=100&include_retried=false")
	link := func(page int, rel string) string {
		return fmt.Sprintf("<https://git.example/api/v4/projects/42/pipelines/90/jobs?page=%d&per_page=100&include_retried=false>; rel=\"%s\"", page, rel)
	}
	for _, tt := range []struct {
		name, target string
		extra        http.Header
	}{
		{"last proves unread page", link(2, "last"), nil},
		{"contradictory first last", link(2, "first last"), nil},
		{"prev on first page", link(2, "prev next"), nil},
		{"wrong first", link(2, "first"), nil},
		{"foreign last host", `<https://evil.example/?page=2>; rel="last"`, nil},
		{"foreign first query", strings.Replace(link(1, "first"), "include_retried=false", "scope=failed", 1), nil},
		{"duplicate relation", link(1, "first") + ", " + link(1, "first"), nil},
		{"duplicate token", link(2, "next next"), nil},
		{"last conflicts next", link(1, "last") + ", " + link(2, "next"), nil},
		{"last conflicts X next", link(1, "last"), http.Header{"X-Next-Page": {"2"}}},
		{"last conflicts total pages", link(1, "last"), http.Header{"X-Total-Pages": {"2"}}},
		{"X total proves unread", "", http.Header{"X-Total-Pages": {"2"}}},
		{"X next empty contradicts last", link(2, "last"), http.Header{"X-Next-Page": {""}}},
		{"X current wrong", link(1, "last"), http.Header{"X-Page": {"2"}}},
		{"X prev wrong", link(1, "last"), http.Header{"X-Prev-Page": {"1"}}},
		{"X page size wrong", link(1, "last"), http.Header{"X-Per-Page": {"20"}}},
		{"X total duplicate", "", http.Header{"X-Total-Pages": {"1", "2"}}},
		{"X zero next is not a page", "", http.Header{"X-Next-Page": {"0"}}},
		{"X zero total contradicts next", link(2, "next"), http.Header{"X-Total": {"0"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := tt.extra.Clone()
			if h == nil {
				h = make(http.Header)
			}
			if tt.target != "" {
				h.Set("Link", tt.target)
			}
			next, err := paginationNext(h, current)
			if err == nil {
				t.Fatalf("ambiguous or incomplete pagination accepted: next=%q", next)
			}
			checkError(t, err, Response)
		})
	}
	// A valid terminal page can carry all four metadata families together.
	terminal, _ := url.Parse(strings.Replace(current.String(), "page=1&", "page=2&", 1))
	h := http.Header{"Link": {link(1, "first prev") + ", " + link(2, "last")}, "X-Page": {"2"}, "X-Prev-Page": {"1"}, "X-Next-Page": {""}, "X-Per-Page": {"100"}, "X-Total-Pages": {"2"}}
	if next, err := paginationNext(h, terminal); err != nil || next != "" {
		t.Fatalf("valid terminal metadata: %q %v", next, err)
	}
}

func TestLastLinkCannotProduceFalseSuccess(t *testing.T) {
	calls := 0
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Link", "<https://"+r.Host+r.URL.Path+"?page=2&per_page=100&include_retried=false>; rel=\"last\"")
		fmt.Fprint(w, `[{"id":1,"status":"success","pipeline":{"id":90,"project_id":42,"sha":"abc"}}]`)
	})
	jobs, err := c.Jobs(context.Background(), 90)
	checkError(t, err, Response)
	if jobs != nil || calls != 1 {
		t.Fatalf("incomplete jobs retained: calls=%d rows=%d", calls, len(jobs))
	}
}

func TestJobsIncludesFailedTrigger(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(strconv.FormatBool(legacy), func(t *testing.T) {
			var paths []string
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				if r.Method != "GET" || r.URL.Query().Get("per_page") != "100" {
					t.Error("unbounded or non-read request")
				}
				switch {
				case strings.HasSuffix(r.URL.Path, "/jobs"):
					fmt.Fprint(w, `[{"id":1,"status":"success","pipeline":{"id":90,"project_id":42,"sha":"abc"}}]`)
				case strings.HasSuffix(r.URL.Path, "/trigger_jobs") && legacy:
					http.NotFound(w, r)
				case strings.HasSuffix(r.URL.Path, "/trigger_jobs"), strings.HasSuffix(r.URL.Path, "/bridges"):
					fmt.Fprintf(w, `[{"id":2,"name":%q,"status":"failed","allow_failure":false,"pipeline":{"id":90,"project_id":42,"sha":"abc"},"downstream_pipeline":{"id":999,"project_id":999,"web_url":"https://evil.example/"}}]`, sentinel)
				default:
					t.Error("scope widened", r.URL.Path)
					http.NotFound(w, r)
				}
			})
			jobs, err := c.Jobs(context.Background(), 90)
			if err != nil {
				t.Fatal(err)
			}
			e := AssessCI(Pipeline{ID: 90, ProjectID: 42, SHA: "abc", Status: "running"}, jobs)
			if e.Verdict != CIFailed || !reflect.DeepEqual(e.FailedJobIDs, []int64{2}) {
				t.Fatalf("trigger omitted: paths=%v evidence=%+v", paths, e)
			}
			data, _ := json.Marshal(jobs)
			if strings.Contains(string(data), sentinel) {
				t.Fatal("trigger token reflection")
			}
			want := 2
			if legacy {
				want = 3
			}
			if len(paths) != want {
				t.Fatalf("unexpected traversal: %v", paths)
			}
		})
	}
}

func TestTriggerEndpointFailureDiscardsBuilds(t *testing.T) {
	for _, status := range []int{401, 403, 404, 429, 500, 503} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			calls := 0
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if strings.HasSuffix(r.URL.Path, "/jobs") {
					fmt.Fprint(w, `[{"id":1,"pipeline":{"id":90,"project_id":42}}]`)
					return
				}
				w.WriteHeader(status)
				fmt.Fprint(w, sentinel)
			})
			jobs, err := c.Jobs(context.Background(), 90)
			checkError(t, err, HTTP)
			want := 2
			if status == 404 {
				want = 3
			}
			if jobs != nil || calls != want {
				t.Fatalf("partial result or unsafe fallback: calls=%d rows=%d", calls, len(jobs))
			}
		})
	}
}

func TestBuildAndTriggerShareBudgets(t *testing.T) {
	for _, budget := range []string{"pages", "bytes", "deadline"} {
		t.Run(budget, func(t *testing.T) {
			calls := 0
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				build := strings.HasSuffix(r.URL.Path, "/jobs")
				page, _ := strconv.Atoi(r.URL.Query().Get("page"))
				if budget == "deadline" {
					select {
					case <-time.After(60 * time.Millisecond):
					case <-r.Context().Done():
						return
					}
				}
				last := 1
				if budget == "pages" {
					last = 6
				}
				if budget == "bytes" {
					last = 3
				}
				if page < last {
					w.Header().Set("X-Next-Page", strconv.Itoa(page+1))
				}
				padding := ""
				if budget == "bytes" {
					padding = strings.Repeat("x", 3<<20)
				}
				id := page
				if !build {
					id += 100
				}
				fmt.Fprintf(w, `[{"id":%d,"name":%q,"pipeline":{"id":90,"project_id":42}}]`, id, padding)
			})
			if budget == "deadline" {
				c.timeout = 100 * time.Millisecond
			}
			jobs, err := c.Jobs(context.Background(), 90)
			kind := Limit
			if budget == "deadline" {
				kind = Timeout
			}
			checkError(t, err, kind)
			if jobs != nil {
				t.Fatal("partial evidence returned")
			}
			if budget == "pages" && calls != MaxPages {
				t.Fatalf("page budget reset between endpoints: %d", calls)
			}
			if budget == "bytes" && calls != 6 {
				t.Fatalf("byte budget reset between endpoints: %d", calls)
			}
		})
	}
}
