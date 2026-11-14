package gitlab

// FEATURE: One-shot, same-project merge request creation.
// SPEC: Validate a caller-authorized target and bounded input before sending;
// verify project/branches/SHA, never replay a POST, and preserve uncertain effects
// without exposing upstream text or treating creation as merge approval.
// IMPLEMENTATION: A fixed POST has no replayable body; bounded validation,
// project/branch/SHA checks and static effect-aware errors surround dispatch.
// RESULT: IMPLEMENTED - TLS fixtures prove the creation and refusal contracts.
// STATUS: DONE

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const createSHA = "0123456789abcdef0123456789abcdef01234567"

func createInput() CreateMergeRequestInput {
	return CreateMergeRequestInput{
		SourceBranch: "feature/302-topic", TargetBranch: "main", SourceSHA: createSHA,
		Title: "Add feature #302", Description: "Implementation details.\nSecond line.", RequestID: "brood-302-request-1",
	}
}

func createdMR() MergeRequest {
	in := createInput()
	return MergeRequest{ID: 80, IID: 8, ProjectID: 42, SourceProjectID: 42, TargetProjectID: 42,
		SourceBranch: in.SourceBranch, TargetBranch: in.TargetBranch, SHA: in.SourceSHA,
		Title: in.Title, Description: in.Description, State: "opened"}
}

func writeCreated(w http.ResponseWriter, mr MergeRequest) {
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(mr)
}

func checkCreateError(t *testing.T, err error, kind ErrorKind, effect CreateEffect, status int) {
	t.Helper()
	var e *CreateError
	if !errors.As(err, &e) || e.Kind != kind || e.Effect != effect || e.Status != status {
		t.Fatalf("want %s/%s/%d, got %#v", kind, effect, status, err)
	}
	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
			text := fmt.Sprintf(verb, cause)
			if strings.Contains(text, sentinel) || strings.Contains(text, "UPSTREAM_PRIVATE") {
				t.Fatalf("upstream data exposed by %s", verb)
			}
		}
		data, _ := json.Marshal(cause)
		if strings.Contains(string(data), sentinel) || strings.Contains(string(data), "UPSTREAM_PRIVATE") {
			t.Fatal("error JSON exposes upstream data")
		}
	}
	var upstream *url.Error
	if errors.As(err, &upstream) || errors.Unwrap(err) != nil {
		t.Fatal("creation error retains upstream error tree")
	}
}

func TestCreateMergeRequest(t *testing.T) {
	var calls atomic.Int32
	in := createInput()
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "POST" || r.URL.Path != "/api/v4/projects/42/merge_requests" || r.URL.RawQuery != "" {
			t.Error("unexpected endpoint", r.Method, r.URL)
		}
		if r.Header.Get("PRIVATE-TOKEN") != sentinel || r.Header.Get("X-Request-ID") != in.RequestID || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Authorization") != "" || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-Idempotency-Key") != "" {
			t.Error("unsafe credentials or correlation metadata")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if len(body) != 4 || body["source_branch"] != in.SourceBranch || body["target_branch"] != "main" || body["title"] != in.Title || body["description"] != in.Description+"\n\n<!-- brood-mr-create:"+in.RequestID+" -->" {
			t.Error("unexpected creation payload", body)
		}
		mr := createdMR()
		mr.Title += sentinel
		mr.Description += sentinel
		mr.WebURL = "https://example.invalid/" + sentinel
		writeCreated(w, mr)
	})
	mr, err := c.CreateMergeRequest(context.Background(), "main", in)
	if err != nil || mr.ID != 80 || mr.IID != 8 || mr.ProjectID != 42 || mr.SourceBranch != in.SourceBranch || mr.TargetBranch != "main" || mr.SHA != createSHA || calls.Load() != 1 {
		t.Fatalf("creation: %+v %v, calls %d", mr, err, calls.Load())
	}
	for _, v := range []any{mr, &mr} {
		b, _ := json.Marshal(v)
		if strings.Contains(fmt.Sprintf("%+v %#v %s", v, v, b), sentinel) {
			t.Fatal("creation DTO leaks reflected credential")
		}
	}
}

func TestCreateValidationNeverSends(t *testing.T) {
	var calls atomic.Int32
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); writeCreated(w, createdMR()) })
	cases := []struct {
		name   string
		change func(*CreateMergeRequestInput)
	}{
		{"empty source", func(in *CreateMergeRequestInput) { in.SourceBranch = "" }},
		{"empty target", func(in *CreateMergeRequestInput) { in.TargetBranch = "" }},
		{"other target", func(in *CreateMergeRequestInput) { in.TargetBranch = "release" }},
		{"same branches", func(in *CreateMergeRequestInput) { in.SourceBranch = "main" }},
		{"empty title", func(in *CreateMergeRequestInput) { in.Title = " \t" }},
		{"long title", func(in *CreateMergeRequestInput) { in.Title = strings.Repeat("x", MaxCreateTitleBytes+1) }},
		{"title control", func(in *CreateMergeRequestInput) { in.Title = "hello\n/merge" }},
		{"title utf8", func(in *CreateMergeRequestInput) { in.Title = string([]byte{0xff}) }},
		{"long description", func(in *CreateMergeRequestInput) { in.Description = strings.Repeat("x", MaxCreateDescriptionBytes+1) }},
		{"description utf8", func(in *CreateMergeRequestInput) { in.Description = string([]byte{0xff}) }},
		{"description nul", func(in *CreateMergeRequestInput) { in.Description = "a\x00b" }},
		{"quick action", func(in *CreateMergeRequestInput) { in.Description = "Text\n \t/merge" }},
		{"quick action cr", func(in *CreateMergeRequestInput) { in.Description = "Text\r/approve" }},
		{"forged marker", func(in *CreateMergeRequestInput) { in.Description = "<!-- brood-mr-create:another -->" }},
		{"empty correlation", func(in *CreateMergeRequestInput) { in.RequestID = "" }},
		{"correlation injection", func(in *CreateMergeRequestInput) { in.RequestID = "id\r\nX: bad" }},
		{"long correlation", func(in *CreateMergeRequestInput) { in.RequestID = strings.Repeat("x", MaxCreateRequestIDBytes+1) }},
		{"missing sha", func(in *CreateMergeRequestInput) { in.SourceSHA = "" }},
		{"short sha", func(in *CreateMergeRequestInput) { in.SourceSHA = "abc" }},
		{"bad sha", func(in *CreateMergeRequestInput) { in.SourceSHA = strings.Repeat("g", 40) }},
		{"credential input", func(in *CreateMergeRequestInput) { in.Description = sentinel }},
	}
	for _, branch := range []string{"-option", "HEAD", "/foo", "foo/", "a//b", "a..b", "a@{b", "a.lock", "a/.b", "a./b", "a b", "a?b", "a#b", "a\\b", "a:b", "refs/heads/foo", strings.Repeat("a", 40), strings.Repeat("F", 40), strings.Repeat("b", MaxCreateBranchBytes+1)} {
		cases = append(cases, struct {
			name   string
			change func(*CreateMergeRequestInput)
		}{"branch " + branch, func(in *CreateMergeRequestInput) { in.SourceBranch = branch }})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := createInput()
			tc.change(&in)
			mr, err := c.CreateMergeRequest(context.Background(), "main", in)
			checkCreateError(t, err, Invalid, CreateNotSent, 0)
			if mr != (MergeRequest{}) {
				t.Fatal("partial result")
			}
		})
	}
	for _, target := range []string{"", "bad..branch", "-bad", sentinel} {
		_, err := c.CreateMergeRequest(context.Background(), target, createInput())
		checkCreateError(t, err, Invalid, CreateNotSent, 0)
	}
	if calls.Load() != 0 {
		t.Fatal("validation sent a request")
	}
}

func TestCreateInputBoundaries(t *testing.T) {
	in := createInput()
	in.SourceBranch = strings.Repeat("a", MaxCreateBranchBytes)
	in.Title = strings.Repeat("x", MaxCreateTitleBytes)
	in.Description = strings.Repeat("x", MaxCreateDescriptionBytes)
	in.RequestID = strings.Repeat("r", MaxCreateRequestIDBytes)
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["source_branch"] != in.SourceBranch || !strings.HasPrefix(body["description"], in.Description) {
			t.Error("boundary input changed")
		}
		mr := createdMR()
		mr.SourceBranch = in.SourceBranch
		writeCreated(w, mr)
	})
	if _, err := c.CreateMergeRequest(context.Background(), "main", in); err != nil {
		t.Fatal(err)
	}
	// An explicitly empty description is permitted by GitLab and still gets metadata.
	in = createInput()
	in.Description = ""
	if _, err := c.CreateMergeRequest(context.Background(), "main", in); err != nil {
		t.Fatal(err)
	}
}

func TestCreateHTTPFailuresAreUncertainAndNeverReplay(t *testing.T) {
	for _, status := range []int{200, 202, 301, 302, 303, 307, 308, 400, 401, 403, 404, 408, 409, 422, 429, 500, 502, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Location", "/api/v4/projects/43/merge_requests")
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(status)
				fmt.Fprint(w, sentinel+" UPSTREAM_PRIVATE")
			})
			mr, err := c.CreateMergeRequest(context.Background(), "main", createInput())
			checkCreateError(t, err, HTTP, CreateUncertain, status)
			if mr != (MergeRequest{}) || calls.Load() != 1 {
				t.Fatalf("partial result or replay, calls=%d", calls.Load())
			}
		})
	}
}

func TestCreateLostReplyNeverReplays(t *testing.T) {
	var posts atomic.Int32
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			fmt.Fprint(w, `{"id":42}`)
			return
		}
		posts.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close() // A create may have committed before the reply was lost.
	})
	if _, err := c.Project(context.Background()); err != nil {
		t.Fatal(err)
	} // Reused idle connection.
	mr, err := c.CreateMergeRequest(context.Background(), "main", createInput())
	checkCreateError(t, err, Transport, CreateUncertain, 0)
	if mr != (MergeRequest{}) || posts.Load() != 1 {
		t.Fatal("POST replayed or partial result")
	}
}

func TestCreateSuccessMustBeCompleteAndBound(t *testing.T) {
	cases := []struct {
		name   string
		change func(*MergeRequest)
	}{
		{"missing ID", func(m *MergeRequest) { m.ID = 0 }},
		{"missing IID", func(m *MergeRequest) { m.IID = 0 }},
		{"foreign project", func(m *MergeRequest) { m.ProjectID = 43 }},
		{"fork", func(m *MergeRequest) { m.SourceProjectID = 43 }},
		{"foreign target", func(m *MergeRequest) { m.TargetProjectID = 43 }},
		{"missing project", func(m *MergeRequest) { m.ProjectID = 0 }},
		{"source branch", func(m *MergeRequest) { m.SourceBranch = "other" }},
		{"target branch", func(m *MergeRequest) { m.TargetBranch = "release" }},
		{"missing sha", func(m *MergeRequest) { m.SHA = "" }},
		{"changed head", func(m *MergeRequest) { m.SHA = strings.Repeat("a", 40) }},
		{"merged", func(m *MergeRequest) { m.State = "merged" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				mr := createdMR()
				tc.change(&mr)
				writeCreated(w, mr)
			})
			mr, err := c.CreateMergeRequest(context.Background(), "main", createInput())
			checkCreateError(t, err, Binding, CreateUncertain, 201)
			if mr != (MergeRequest{}) || calls.Load() != 1 {
				t.Fatal("foreign result accepted or replayed")
			}
		})
	}
	for _, tc := range []struct {
		name, body string
		length     int
		kind       ErrorKind
	}{
		{"malformed", `{"id":80,`, 0, Response},
		{"null", `null`, 0, Binding},
		{"truncated", `{"id":80}`, 1000, Response},
		{"oversized", strings.Repeat(" ", MaxResponseBytes+1), 0, Limit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if tc.length != 0 {
					w.Header().Set("Content-Length", fmt.Sprint(tc.length))
				}
				w.WriteHeader(201)
				fmt.Fprint(w, tc.body)
			})
			mr, err := c.CreateMergeRequest(context.Background(), "main", createInput())
			checkCreateError(t, err, tc.kind, CreateUncertain, 201)
			if mr != (MergeRequest{}) || calls.Load() != 1 {
				t.Fatal("partial result or replay")
			}
		})
	}
}

func TestCreateCancellationEffect(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprint(expired), func(t *testing.T) {
			var calls atomic.Int32
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
			ctx, cancel := context.WithCancel(context.Background())
			kind, target := Canceled, error(context.Canceled)
			if expired {
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				kind, target = Timeout, context.DeadlineExceeded
			}
			cancel()
			_, err := c.CreateMergeRequest(ctx, "main", createInput())
			checkCreateError(t, err, kind, CreateNotSent, 0)
			if !errors.Is(err, target) || calls.Load() != 0 {
				t.Fatal("cancellation semantics or pre-send effect incorrect")
			}
		})
	}
	t.Run("after dispatch", func(t *testing.T) {
		var calls atomic.Int32
		c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
		})
		c.timeout = 2 * time.Second
		_, err := c.CreateMergeRequest(context.Background(), "main", createInput())
		checkCreateError(t, err, Timeout, CreateUncertain, 0)
		if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 {
			t.Fatal("post-dispatch timeout lost uncertainty")
		}
	})
}
