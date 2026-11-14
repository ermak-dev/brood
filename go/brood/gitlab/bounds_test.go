package gitlab

// FEATURE: Adversarial read boundaries and pagination metadata.
// SPEC: A Link next page cannot hide incomplete evidence or redirect reads;
// all list/hydration failures discard prefixes and operation deadlines persist.
// IMPLEMENTATION: Validate next metadata against local numeric URLs; share an
// explicit byte budget across every page, MR binding read and hydration.
// RESULT: IMPLEMENTED - Short-page Link and aggregate-budget regressions pass.
// STATUS: DONE

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLinkPaginationUsesOnlyBoundLocalPages(t *testing.T) {
	for _, tt := range []struct {
		name  string
		kind  ErrorKind
		pages int
	}{
		{"valid", "", 3}, {"foreign host", Response, 1}, {"foreign project", Response, 1}, {"foreign query", Response, 1}, {"credentials", Response, 1}, {"malformed", Response, 1}, {"conflicting header", Response, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if strings.HasSuffix(r.URL.Path, "/trigger_jobs") {
					fmt.Fprint(w, `[]`)
					return
				}
				if calls == 1 {
					link := "https://" + r.Host + r.URL.Path + "?page=2&per_page=100&include_retried=false"
					switch tt.name {
					case "foreign host":
						link = "https://evil.example/" + sentinel
					case "foreign project":
						link = strings.Replace(link, "projects/42", "projects/43", 1)
					case "foreign query":
						link += "&scope=all"
					case "credentials":
						link = strings.Replace(link, "https://", "https://"+sentinel+"@", 1)
					case "conflicting header":
						w.Header().Set("X-Next-Page", "3")
					}
					if tt.name == "malformed" {
						w.Header().Set("Link", "<"+link)
					} else {
						w.Header().Set("Link", "<"+link+">; rel=\"next\"")
					}
				}
				fmt.Fprintf(w, `[{"id":%d,"pipeline":{"id":90,"project_id":42}}]`, calls)
			})
			js, e := c.Jobs(context.Background(), 90)
			if tt.kind != "" {
				checkError(t, e, tt.kind)
				if js != nil {
					t.Fatal("partial page prefix")
				}
			} else if e != nil || len(js) != 2 {
				t.Fatalf("Link pagination was silently truncated: %+v %v", js, e)
			}
			if calls != tt.pages {
				t.Fatalf("requests %d want %d", calls, tt.pages)
			}
		})
	}
}

func TestPaginationRejectsOversizedPageAndDuplicateMetadata(t *testing.T) {
	for _, tt := range []struct {
		name      string
		count     int
		duplicate bool
	}{
		{"too many rows", 101, false}, {"duplicate next headers", 1, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if tt.duplicate {
					w.Header().Add("X-Next-Page", "")
					w.Header().Add("X-Next-Page", "2")
				}
				rows := make([]Job, tt.count)
				for i := range rows {
					rows[i] = Job{ID: int64(i + 1), Pipeline: Pipeline{ID: 90, ProjectID: 42}}
				}
				_ = json.NewEncoder(w).Encode(rows)
			})
			js, e := c.Jobs(context.Background(), 90)
			checkError(t, e, Response)
			if js != nil {
				t.Fatal("partial list")
			}
		})
	}
}

func TestAggregateOperationByteBudget(t *testing.T) {
	// Each response is legal individually, but retaining a long pipeline list
	// must not multiply a per-response bound into gigabytes of DTO strings.
	const payload = 3 << 20
	for _, hydrate := range []bool{false, true} {
		t.Run(strconv.FormatBool(hydrate), func(t *testing.T) {
			calls := 0
			padding := strings.Repeat("x", payload)
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if hydrate {
					switch {
					case strings.HasSuffix(r.URL.Path, "/merge_requests/8"):
						fmt.Fprint(w, `{"id":80,"iid":8,"project_id":42,"source_project_id":42,"target_project_id":42}`)
					case strings.HasSuffix(r.URL.Path, "/pipelines"):
						fmt.Fprint(w, `[{"id":90},{"id":91},{"id":92},{"id":93},{"id":94},{"id":95}]`)
					default:
						id, _ := strconv.ParseInt(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], 10, 64)
						fmt.Fprintf(w, `{"id":%d,"project_id":42,"ref":"%s"}`, id, padding)
					}
					return
				}
				page, _ := strconv.Atoi(r.URL.Query().Get("page"))
				if page < 6 {
					w.Header().Set("X-Next-Page", strconv.Itoa(page+1))
				}
				fmt.Fprintf(w, `[{"id":%d,"name":"%s","pipeline":{"id":90,"project_id":42}}]`, page, padding)
			})
			if hydrate {
				ps, e := c.MergeRequestPipelines(context.Background(), 8)
				checkError(t, e, Limit)
				if ps != nil || calls != 8 {
					t.Fatal("aggregate hydration budget not enforced")
				}
			} else {
				js, e := c.Jobs(context.Background(), 90)
				checkError(t, e, Limit)
				if js != nil || calls != 6 {
					t.Fatal("aggregate pagination budget not enforced")
				}
			}
		})
	}
}

func TestResponseSizeBoundary(t *testing.T) {
	head := `{"id":42,"name":"`
	tail := `"}`
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, head+strings.Repeat("x", MaxResponseBytes-len(head)-len(tail))+tail)
	})
	p, e := c.Project(context.Background())
	if e != nil || len(p.Name) != MaxResponseBytes-len(head)-len(tail) {
		t.Fatalf("exact byte bound refused: %v", e)
	}
}

func TestPrefixedBaseAndUntrustedURLsStayData(t *testing.T) {
	var calls atomic.Int32
	c, s := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.EscapedPath() != "/gitlab/api/v4/projects/42/issues/9223372036854775807" || r.URL.RawQuery != "" {
			t.Errorf("unexpected escaped target: %s", r.URL.String())
		}
		fmt.Fprint(w, `{"id":1,"iid":9223372036854775807,"project_id":42,"web_url":"https://evil.example/?token=anything","title":"../other?query#fragment"}`)
	})
	// Equivalent constructor with the same trusted test certificate and no other
	// transport changes: a reverse-proxy prefix must survive path construction.
	prefixed, e := NewClient(sentinel, Config{APIBase: s.URL + "/gitlab/api/v4/", ProjectID: 42})
	if e != nil {
		t.Fatal(e)
	}
	defer prefixed.Close()
	prefixed.http.Transport.(*http.Transport).TLSClientConfig.RootCAs = c.http.Transport.(*http.Transport).TLSClientConfig.RootCAs
	i, e := prefixed.Issue(context.Background(), 1<<63-1)
	if e != nil || i.Title != "../other?query#fragment" || calls.Load() != 1 {
		t.Fatalf("read: %+v %v", i, e)
	}
}

func TestMRPipelineLateFailureDiscardsHydratedPrefix(t *testing.T) {
	calls := 0
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch r.URL.Path {
		case "/api/v4/projects/42/merge_requests/8":
			fmt.Fprint(w, `{"id":80,"iid":8,"project_id":42,"source_project_id":42,"target_project_id":42}`)
		case "/api/v4/projects/42/merge_requests/8/pipelines":
			fmt.Fprint(w, `[{"id":90},{"id":91}]`)
		case "/api/v4/projects/42/pipelines/90":
			fmt.Fprint(w, `{"id":90,"project_id":42}`)
		default:
			w.WriteHeader(404)
			fmt.Fprint(w, sentinel)
		}
	})
	ps, e := c.MergeRequestPipelines(context.Background(), 8)
	checkError(t, e, HTTP)
	if ps != nil || calls != 4 {
		t.Fatal("hydration returned partial evidence")
	}
}

func TestBodyDeadlineAndSharedHydrationBudget(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":42`)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	c.timeout = 20 * time.Millisecond
	_, e := c.Project(context.Background())
	checkError(t, e, Timeout)
	c2, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(30 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/merge_requests/8"):
			fmt.Fprint(w, `{"id":80,"iid":8,"project_id":42,"source_project_id":42,"target_project_id":42}`)
		case strings.HasSuffix(r.URL.Path, "/pipelines"):
			fmt.Fprint(w, `[{"id":90},{"id":91}]`)
		default:
			id, _ := strconv.ParseInt(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], 10, 64)
			fmt.Fprintf(w, `{"id":%d,"project_id":42}`, id)
		}
	})
	c2.timeout = 75 * time.Millisecond
	ps, e := c2.MergeRequestPipelines(context.Background(), 8)
	checkError(t, e, Timeout)
	if ps != nil {
		t.Fatal("hydration prefix returned after deadline")
	}
}
