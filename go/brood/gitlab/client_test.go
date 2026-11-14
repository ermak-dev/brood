package gitlab

// FEATURE: Project-bound, read-only GitLab API client.
// SPEC: Explicit HTTPS API base, numeric binding and supplied token only; bounded
// reads never follow redirects/proxies or return credentials or partial results.
// IMPLEMENTATION: Fixed numeric paths, one deadline, bounded pages/JSON, static
// errors and decoded-field redaction keep untrusted replies out of credentials.
// RESULT: IMPLEMENTED - TLS fixtures cover typed reads and refusal boundaries.
// STATUS: DONE

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const sentinel = "glpat-DO_NOT_RETURN_SENTINEL_302"

func testClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	s := httptest.NewTLSServer(handler)
	t.Cleanup(s.Close)
	c, err := NewClient(sentinel, Config{APIBase: s.URL + "/api/v4", ProjectID: 42})
	if err != nil {
		t.Fatal(err)
	}
	// Trust only this test's certificate; retain the production transport policy.
	roots := x509.NewCertPool()
	roots.AddCert(s.Certificate())
	c.http.Transport.(*http.Transport).TLSClientConfig.RootCAs = roots
	t.Cleanup(c.Close)
	return c, s
}

func checkError(t *testing.T, err error, kind ErrorKind) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Kind != kind {
		t.Fatalf("want kind %s, got %#v", kind, err)
	}
	for _, f := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		if strings.Contains(fmt.Sprintf(f, err), sentinel) {
			t.Fatalf("error leaks token with %s", f)
		}
	}
	if errors.Unwrap(err) != nil {
		t.Fatal("error exposes an upstream cause")
	}
	var upstream *url.Error
	if errors.As(err, &upstream) {
		t.Fatal("error exposes an HTTP request")
	}
	b, _ := json.Marshal(e)
	if strings.Contains(string(b), sentinel) {
		t.Fatal("error JSON leaks token")
	}
}

func TestExplicitConfiguration(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", sentinel)
	for _, base := range []string{"", "http://git.example/api/v4", "https://user:pass@git.example/api/v4", "https://git.example/api/v4?token=" + sentinel, "https://git.example/api/v4#x", "https://git.example", "https://git.example/api/v4/../v4", "https://git.example/%61pi/v4", "https://git.example:0/api/v4", "https://git.example:65536/api/v4", "https://git.example/api/v4?", "https://git.example/api/v4#", "https://git.example\\evil/api/v4"} {
		t.Run(base, func(t *testing.T) {
			c, err := NewClient(sentinel, Config{APIBase: base, ProjectID: 42})
			if c != nil {
				c.Close()
				t.Fatal("accepted unsafe API base")
			}
			checkError(t, err, Invalid)
		})
	}
	for _, token := range []string{"", "bad\n" + sentinel, "bad token", strings.Repeat("x", 4097)} {
		_, err := NewClient(token, Config{APIBase: "https://git.example/api/v4", ProjectID: 42})
		checkError(t, err, Invalid)
	}
	for _, id := range []int64{0, -1} {
		_, err := NewClient(sentinel, Config{APIBase: "https://git.example/api/v4", ProjectID: id})
		checkError(t, err, Invalid)
	}
	for _, timeout := range []time.Duration{-1, MaxTimeout + 1} {
		_, err := NewClient(sentinel, Config{APIBase: "https://git.example/api/v4", ProjectID: 42, Timeout: timeout})
		checkError(t, err, Invalid)
	}
	c, err := NewClient(sentinel, Config{APIBase: "https://git.example/gitlab/api/v4/", ProjectID: 42})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.http.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("environment proxy enabled")
	}
	for _, v := range []any{c, *c, struct{ C Client }{*c}} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
			if strings.Contains(fmt.Sprintf(verb, v), sentinel) {
				t.Fatalf("client formatting leaks token: %s", verb)
			}
		}
		b, err := json.Marshal(v)
		if err != nil || strings.Contains(string(b), sentinel) {
			t.Fatal("client JSON leaks token")
		}
	}
}

func TestTypedReadEndpoints(t *testing.T) {
	var paths []string
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.EscapedPath())
		if r.Method != "GET" || r.Header.Get("PRIVATE-TOKEN") != sentinel || r.Header.Get("Authorization") != "" || strings.Contains(r.URL.String(), sentinel) {
			t.Error("unsafe request")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v4/projects/42":
			fmt.Fprint(w, `{"id":42,"name":"Example","path_with_namespace":"group/project","default_branch":"main"}`)
		case "/api/v4/projects/42/issues/7":
			fmt.Fprint(w, `{"id":70,"iid":7,"project_id":42,"title":"Issue","description":"body","state":"opened"}`)
		case "/api/v4/projects/42/merge_requests/8":
			fmt.Fprint(w, `{"id":80,"iid":8,"project_id":42,"source_project_id":42,"target_project_id":42,"title":"MR","sha":"abc","source_branch":"topic/a?b#c","target_branch":"main","draft":true}`)
		case "/api/v4/projects/42/merge_requests/8/pipelines":
			if r.URL.Query().Get("page") != "1" || r.URL.Query().Get("per_page") != "100" {
				t.Error("missing bounded pagination")
			}
			fmt.Fprint(w, `[{"id":90,"sha":"abc","ref":"topic/a?b#c","status":"running"}]`)
		case "/api/v4/projects/42/pipelines/90":
			fmt.Fprint(w, `{"id":90,"project_id":42,"sha":"abc","ref":"topic/a?b#c","status":"running","yaml_errors":null}`)
		case "/api/v4/projects/42/pipelines/90/trigger_jobs":
			fmt.Fprint(w, `[]`)
		case "/api/v4/projects/42/pipelines/90/jobs":
			if r.URL.Query().Get("include_retried") != "false" {
				t.Error("must request current jobs")
			}
			fmt.Fprint(w, `[{"id":100,"name":"unit","stage":"test","status":"failed","allow_failure":false,"pipeline":{"id":90,"project_id":42,"sha":"abc"}}]`)
		default:
			t.Error("unexpected endpoint", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	ctx := context.Background()
	p, err := c.Project(ctx)
	if err != nil || p.ID != 42 || p.PathWithNamespace != "group/project" {
		t.Fatalf("project: %+v %v", p, err)
	}
	i, err := c.Issue(ctx, 7)
	if err != nil || i.IID != 7 || i.Description != "body" {
		t.Fatalf("issue: %+v %v", i, err)
	}
	mr, err := c.MergeRequest(ctx, 8)
	if err != nil || mr.IID != 8 || mr.SourceBranch != "topic/a?b#c" || !mr.Draft {
		t.Fatalf("MR: %+v %v", mr, err)
	}
	ps, err := c.MergeRequestPipelines(ctx, 8)
	if err != nil || len(ps) != 1 || ps[0].ProjectID != 42 {
		t.Fatalf("MR pipelines: %+v %v", ps, err)
	}
	pipe, err := c.Pipeline(ctx, 90)
	if err != nil || pipe.Status != "running" {
		t.Fatalf("pipeline: %+v %v", pipe, err)
	}
	jobs, err := c.Jobs(ctx, 90)
	if err != nil || len(jobs) != 1 || jobs[0].Name != "unit" || jobs[0].AllowFailure {
		t.Fatalf("jobs: %+v %v", jobs, err)
	}
	if len(paths) != 9 {
		t.Fatalf("expected MR binding check and pipeline hydration: %v", paths)
	}
}

func TestInvalidIDsDoNotSend(t *testing.T) {
	var calls atomic.Int32
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); fmt.Fprint(w, `{}`) })
	for _, id := range []int64{0, -3} {
		_, e := c.Issue(context.Background(), id)
		checkError(t, e, Invalid)
		_, e = c.MergeRequest(context.Background(), id)
		checkError(t, e, Invalid)
		_, e = c.MergeRequestPipelines(context.Background(), id)
		checkError(t, e, Invalid)
		_, e = c.Pipeline(context.Background(), id)
		checkError(t, e, Invalid)
		_, e = c.Jobs(context.Background(), id)
		checkError(t, e, Invalid)
	}
	if calls.Load() != 0 {
		t.Fatal("invalid IDs sent requests")
	}
}

func TestBindingRejectsForeignOrMissingIDs(t *testing.T) {
	cases := []struct {
		name, body string
		read       func(*Client) error
	}{
		{"project", `{"id":43}`, func(c *Client) error { _, e := c.Project(context.Background()); return e }},
		{"issue project", `{"id":1,"iid":7,"project_id":43}`, func(c *Client) error { _, e := c.Issue(context.Background(), 7); return e }},
		{"issue iid", `{"id":1,"iid":8,"project_id":42}`, func(c *Client) error { _, e := c.Issue(context.Background(), 7); return e }},
		{"issue missing project", `{"id":1,"iid":7}`, func(c *Client) error { _, e := c.Issue(context.Background(), 7); return e }},
		{"MR fork", `{"id":1,"iid":8,"project_id":42,"source_project_id":43,"target_project_id":42}`, func(c *Client) error { _, e := c.MergeRequest(context.Background(), 8); return e }},
		{"MR target", `{"id":1,"iid":8,"project_id":42,"source_project_id":42,"target_project_id":43}`, func(c *Client) error { _, e := c.MergeRequest(context.Background(), 8); return e }},
		{"pipeline project", `{"id":90,"project_id":43}`, func(c *Client) error { _, e := c.Pipeline(context.Background(), 90); return e }},
		{"pipeline id", `{"id":91,"project_id":42}`, func(c *Client) error { _, e := c.Pipeline(context.Background(), 90); return e }},
		{"job project", `[{"id":1,"pipeline":{"id":90,"project_id":43}}]`, func(c *Client) error { _, e := c.Jobs(context.Background(), 90); return e }},
		{"job pipeline", `[{"id":1,"pipeline":{"id":91,"project_id":42}}]`, func(c *Client) error { _, e := c.Jobs(context.Background(), 90); return e }},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tt.body) })
			checkError(t, tt.read(c), Binding)
		})
	}
}

func TestMRPipelineHydrationRejectsForeignProject(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/projects/42/merge_requests/8":
			fmt.Fprint(w, `{"id":80,"iid":8,"project_id":42,"source_project_id":42,"target_project_id":42}`)
		case "/api/v4/projects/42/merge_requests/8/pipelines":
			fmt.Fprint(w, `[{"id":90}]`)
		default:
			fmt.Fprint(w, `{"id":90,"project_id":43}`)
		}
	})
	ps, err := c.MergeRequestPipelines(context.Background(), 8)
	checkError(t, err, Binding)
	if ps != nil {
		t.Fatal("returned unverified pipeline")
	}
}

func TestPaginationAndNoPartialResults(t *testing.T) {
	for _, tt := range []struct {
		name, next string
		want       ErrorKind
		pages      int
	}{
		{"normal", "2", "", 3}, {"no next header", "", "", 3}, {"cycle", "1", Response, 1}, {"jump", "3", Response, 1}, {"foreign URL", "https://evil.example/" + sentinel, Response, 1}, {"page cap", "2", Limit, MaxPages}, {"late failure", "2", HTTP, 2}, {"duplicate", "2", Response, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if strings.HasSuffix(r.URL.Path, "/trigger_jobs") {
					fmt.Fprint(w, `[]`)
					return
				}
				page, _ := strconv.Atoi(r.URL.Query().Get("page"))
				if page != calls {
					t.Error("wrong page", page, calls)
				}
				if tt.name == "late failure" && page == 2 {
					w.WriteHeader(503)
					fmt.Fprint(w, sentinel)
					return
				}
				if page == 1 {
					w.Header().Set("X-Next-Page", tt.next)
				}
				if tt.name == "page cap" {
					w.Header().Set("X-Next-Page", strconv.Itoa(page+1))
				}
				count := 100
				if page > 1 && tt.name != "page cap" {
					count = 1
				}
				rows := make([]Job, count)
				for i := range rows {
					rows[i] = Job{ID: int64((page-1)*100 + i + 1), Pipeline: Pipeline{ID: 90, ProjectID: 42}}
				}
				if tt.name == "duplicate" && page == 2 {
					rows[0].ID = 1
				}
				_ = json.NewEncoder(w).Encode(rows)
			})
			rows, err := c.Jobs(context.Background(), 90)
			if tt.want != "" {
				checkError(t, err, tt.want)
				if rows != nil {
					t.Fatal("partial results returned")
				}
			} else if err != nil || len(rows) != 101 {
				t.Fatalf("pagination: %d %v", len(rows), err)
			}
			if calls != tt.pages {
				t.Fatalf("requests %d want %d", calls, tt.pages)
			}
		})
	}
}

func TestMalformedTruncatedOversizedResponses(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		kind       ErrorKind
	}{
		{"malformed", `{"id":42,` + sentinel, Response}, {"truncated", `{"id":42`, Response}, {"null", `null`, Binding}, {"trailing", `{"id":42} {}`, Response}, {"oversize", `{"id":42,"description":"` + strings.Repeat("x", MaxResponseBytes) + `"}`, Limit},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tt.body) })
			p, e := c.Project(context.Background())
			checkError(t, e, tt.kind)
			if p != (Project{}) {
				t.Fatal("partial object")
			}
		})
	}
	for _, body := range []string{`null`, `{}`, `[{"id":1,"pipeline":{"id":90,"project_id":42}}`} {
		c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
		js, e := c.Jobs(context.Background(), 90)
		checkError(t, e, Response)
		if js != nil {
			t.Fatal("partial jobs")
		}
	}
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		fmt.Fprint(w, `{"id":42}`)
	})
	_, e := c.Project(context.Background())
	checkError(t, e, Response)
}

func TestHTTPFailuresAndRedirectsNeverLeakOrForward(t *testing.T) {
	var hits atomic.Int32
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer other.Close()
	for _, status := range []int{301, 302, 303, 307, 308, 401, 403, 404, 429, 500, 503} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", other.URL+"/"+sentinel)
				w.WriteHeader(status)
				fmt.Fprint(w, `{"message":"`+sentinel+`"}`)
			})
			_, e := c.Project(context.Background())
			checkError(t, e, HTTP)
			var api *Error
			errors.As(e, &api)
			if api.Status != status {
				t.Fatal("lost HTTP status")
			}
		})
	}
	if hits.Load() != 0 {
		t.Fatal("redirect target contacted")
	}
}

func TestTokenReflectionsAreScrubbedInDTOs(t *testing.T) {
	// BUG: The old escaped-token fixture replaced g with itself.
	// HYPOTHESIS: A real Unicode escape must decode to the sentinel before scrub.
	// SOLUTION: Build the backslash explicitly and verify the fixture's decoding.
	// RESULT: FIXED - Escaped project and nested DTO fields retain sentinel checks.
	// STATUS: FIXED
	escaped := strings.ReplaceAll(sentinel, "g", string(rune(92))+"u0067")
	var decoded string
	if escaped == sentinel || json.Unmarshal([]byte(`"`+escaped+`"`), &decoded) != nil || decoded != sentinel {
		t.Fatal("fixture must contain an actual JSON escape of the token")
	}
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":42,"name":"%s","path_with_namespace":"group/%s","default_branch":"%s","web_url":"https://example/%s"}`, escaped, sentinel, sentinel, sentinel)
	})
	p, e := c.Project(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(p)
	if strings.Contains(string(b), sentinel) || !strings.Contains(p.Name, "redacted") {
		t.Fatal("token reflected into DTO")
	}
	c2, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/trigger_jobs") {
			fmt.Fprint(w, `[]`)
			return
		}
		fmt.Fprintf(w, `[{"id":1,"name":"%s","stage":"%s","status":"%s","web_url":"%s","pipeline":{"id":90,"project_id":42,"sha":"%s","ref":"%s","status":"%s","web_url":"%s","yaml_errors":"%s","failure_reason":"%s","detailed_status":{"text":"%s"}}}]`, escaped, sentinel, sentinel, sentinel, sentinel, sentinel, sentinel, sentinel, sentinel, sentinel, sentinel)
	})
	jobs, e := c2.Jobs(context.Background(), 90)
	if e != nil {
		t.Fatal(e)
	}
	b, _ = json.Marshal(jobs)
	if strings.Contains(string(b), sentinel) {
		t.Fatal("nested DTO leaks token")
	}
	for _, call := range []string{"issue", "mr"} {
		c3, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"id":1,"iid":8,"project_id":42,"source_project_id":42,"target_project_id":42,"title":"%s","description":"%s","state":"%s","web_url":"%s","sha":"%s","source_branch":"%s","target_branch":"%s"}`, escaped, sentinel, sentinel, sentinel, sentinel, sentinel, sentinel)
		})
		var result any
		if call == "issue" {
			result, e = c3.Issue(context.Background(), 8)
		} else {
			result, e = c3.MergeRequest(context.Background(), 8)
		}
		if e != nil {
			t.Fatal(e)
		}
		b, _ = json.Marshal(result)
		if strings.Contains(string(b), sentinel) {
			t.Fatal("issue/MR DTO leaks token")
		}
	}
}

func TestDeadlinesCancellationAndNoProxy(t *testing.T) {
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { proxied.Add(1) }))
	defer proxy.Close()
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("ALL_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	c.timeout = 25 * time.Millisecond
	started := time.Now()
	_, e := c.Project(context.Background())
	checkError(t, e, Timeout)
	if !errors.Is(e, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatal("deadline not bounded")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, e = c.Project(ctx)
	checkError(t, e, Canceled)
	if !errors.Is(e, context.Canceled) {
		t.Fatal("lost cancellation classification")
	}
	if proxied.Load() != 0 {
		t.Fatal("used environment proxy")
	}
	// One budget spans all pages, not a fresh timeout for every request.
	c2, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(30 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		w.Header().Set("X-Next-Page", strconv.Itoa(page+1))
		fmt.Fprintf(w, `[{"id":%d,"pipeline":{"id":90,"project_id":42}}]`, page)
	})
	c2.timeout = 70 * time.Millisecond
	_, e = c2.Jobs(context.Background(), 90)
	checkError(t, e, Timeout)
}

func TestEmptyListsAndReadOnlySurface(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `[]`) })
	js, e := c.Jobs(context.Background(), 90)
	if e != nil || js == nil || len(js) != 0 {
		t.Fatalf("empty jobs %v %v", js, e)
	}
	typ := reflect.TypeOf(c)
	names := make([]string, typ.NumMethod())
	for i := range names {
		names[i] = typ.Method(i).Name
	}
	want := []string{"Close", "Format", "GoString", "Issue", "Jobs", "MergeRequest", "MergeRequestPipelines", "Pipeline", "Project", "String"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("unexpected public client surface: %v", names)
	}
}
