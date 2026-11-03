package server

import (
	"bytes"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// gitCapture records every gateway response sent to the guest Git client.
type gitCapture struct {
	mu      sync.Mutex
	headers []http.Header
	body    bytes.Buffer
}

type gitCaptureWriter struct {
	http.ResponseWriter
	capture *gitCapture
	wrote   bool
}

func (w *gitCaptureWriter) WriteHeader(code int) {
	if !w.wrote {
		w.wrote = true
		w.capture.mu.Lock()
		w.capture.headers = append(w.capture.headers, w.Header().Clone())
		w.capture.mu.Unlock()
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *gitCaptureWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	w.capture.mu.Lock()
	w.capture.body.Write(p)
	w.capture.mu.Unlock()
	return w.ResponseWriter.Write(p)
}

func (w *gitCaptureWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *gitCaptureWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// gitCommand runs git with an isolated configuration and no proxies.
func gitCommand(t *testing.T, home, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = []string{
		"HOME=" + home, "PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false", "NO_PROXY=*", "LC_ALL=C",
		"GIT_AUTHOR_NAME=Brood Test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=Brood Test", "GIT_COMMITTER_EMAIL=test@example.invalid",
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func mustGit(t *testing.T, home, dir string, args ...string) string {
	t.Helper()
	out, err := gitCommand(t, home, dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return out
}

// FEATURE: Real smart-HTTP clone and push through the Git gateway (#146).
// SPEC: An unmodified git client with only the guest bearer token clones and
// pushes (including chunked uploads) to the installation remote served by git
// http-backend over TLS. The remote credential reaches the upstream on every
// request and never appears in any response or client error.
// IMPLEMENTATION: Router + reverse proxy to net/http/cgi git http-backend.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestGitGatewayCloneAndPushEndToEnd(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	home := t.TempDir()
	root := t.TempDir()
	bare := filepath.Join(root, "group", "repo.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o700); err != nil {
		t.Fatal(err)
	}
	mustGit(t, home, root, "init", "-q", "--bare", "--initial-branch=main", bare)
	mustGit(t, home, bare, "config", "http.receivepack", "true")
	seed := t.TempDir()
	mustGit(t, home, seed, "init", "-q", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("seeded through the gateway\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustGit(t, home, seed, "add", "README.md")
	mustGit(t, home, seed, "commit", "-q", "-m", "seed")
	mustGit(t, home, seed, "push", "-q", bare, "main")

	f := gitGatewaySetup(t)
	backend := &cgi.Handler{Path: gitPath, Args: []string{"http-backend"}, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "HOME=" + home, "GIT_CONFIG_NOSYSTEM=1"}}
	var chunked atomic.Int32
	f.upstream.setInner(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// net/http/cgi refuses chunked bodies; real Git servers accept them.
		// The upstream, not the gateway, buffers the streamed upload here.
		if r.ContentLength < 0 {
			chunked.Add(1)
			body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
			if err != nil {
				http.Error(w, "upstream read failed", http.StatusBadRequest)
				return
			}
			r.Body, r.ContentLength, r.TransferEncoding = io.NopCloser(bytes.NewReader(body)), int64(len(body)), nil
		}
		backend.ServeHTTP(w, r)
	}))
	capture := &gitCapture{}
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.ts.srv.Handler().ServeHTTP(&gitCaptureWriter{ResponseWriter: w, capture: capture}, r)
	}))
	defer front.Close()
	remote := front.URL + f.base()
	header := "http.extraHeader=Authorization: Bearer " + f.token
	work := filepath.Join(t.TempDir(), "project")
	mustGit(t, home, root, "-c", header, "clone", "-q", remote, work)
	content, err := os.ReadFile(filepath.Join(work, "README.md"))
	if err != nil || string(content) != "seeded through the gateway\n" {
		t.Fatalf("clone content: %q %v", content, err)
	}

	if err := os.WriteFile(filepath.Join(work, "change.txt"), []byte("pushed through the gateway\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustGit(t, home, work, "add", "change.txt")
	mustGit(t, home, work, "commit", "-q", "-m", "small push")
	mustGit(t, home, work, "-c", header, "push", "-q", "origin", "HEAD:main")
	if got := mustGit(t, home, bare, "show", "main:change.txt"); got != "pushed through the gateway\n" {
		t.Fatalf("pushed content: %q", got)
	}

	// Force a chunked receive-pack upload with the probe RPC.
	large := make([]byte, 256<<10)
	if _, err := rand.Read(large); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "large.bin"), large, 0o600); err != nil {
		t.Fatal(err)
	}
	mustGit(t, home, work, "add", "large.bin")
	mustGit(t, home, work, "commit", "-q", "-m", "chunked push")
	mustGit(t, home, work, "-c", header, "-c", "http.postBuffer=4096", "push", "-q", "origin", "HEAD:main")
	if chunked.Load() == 0 {
		t.Fatal("chunked upload did not reach upstream as a stream")
	}
	head := strings.TrimSpace(mustGit(t, home, work, "rev-parse", "HEAD"))
	if got := strings.TrimSpace(mustGit(t, home, bare, "rev-parse", "main")); got != head {
		t.Fatalf("chunked push: upstream main=%s local=%s", got, head)
	}
	fetched := filepath.Join(t.TempDir(), "fetched")
	mustGit(t, home, root, "-c", header, "-c", "protocol.version=2", "clone", "-q", remote, fetched)
	if got := strings.TrimSpace(mustGit(t, home, fetched, "rev-parse", "HEAD")); got != head {
		t.Fatalf("protocol v2 clone: %s want %s", got, head)
	}

	seen := f.upstream.seen()
	operations := map[string]bool{}
	for _, request := range seen {
		user, password, ok := (&http.Request{Header: request.header}).BasicAuth()
		if !ok || user != "brood-bot" || password != gitTokenSentinel {
			t.Fatalf("upstream request without the installation credential: %s %s", request.method, request.path)
		}
		for key, values := range request.header {
			if strings.Contains(strings.Join(values, " "), f.token) {
				t.Fatalf("guest token reached upstream in %s", key)
			}
		}
		operations[request.method+" "+request.path+"?"+request.query] = true
	}
	for _, want := range []string{
		"GET /group/repo.git/info/refs?service=git-upload-pack",
		"POST /group/repo.git/git-upload-pack?",
		"GET /group/repo.git/info/refs?service=git-receive-pack",
		"POST /group/repo.git/git-receive-pack?",
	} {
		if !operations[want] {
			t.Fatalf("operation %q not proxied; saw %v", want, operations)
		}
	}

	// The upstream rejects the installation credential: the client fails
	// without seeing it. A subprocess gives the race detector no ordering, so
	// server state is not mutated here.
	f.upstream.rotate("rotated-upstream-token")
	out, err := gitCommand(t, home, root, "-c", header, "clone", "-q", remote, filepath.Join(t.TempDir(), "denied"))
	if err == nil || strings.Contains(out, gitTokenSentinel) || strings.Contains(out, "brood-bot") || strings.Contains(out, f.token) {
		t.Fatalf("rejected credential: err=%v output=%s", err, out)
	}

	capture.mu.Lock()
	defer capture.mu.Unlock()
	for _, secret := range []string{gitTokenSentinel, "brood-bot", f.token, "cookie-sentinel", "private-header-sentinel"} {
		if bytes.Contains(capture.body.Bytes(), []byte(secret)) {
			t.Fatalf("response body disclosed %q", secret)
		}
	}
	for _, h := range capture.headers {
		for key, values := range h {
			switch http.CanonicalHeaderKey(key) {
			case "Content-Type", "Content-Encoding", "Cache-Control", "X-Content-Type-Options":
			default:
				t.Fatalf("response header %s reached the guest", key)
			}
			for _, secret := range []string{gitTokenSentinel, "brood-bot", f.token, "cookie-sentinel"} {
				if strings.Contains(strings.Join(values, " "), secret) {
					t.Fatalf("response header %s disclosed a secret", key)
				}
			}
		}
	}
}
