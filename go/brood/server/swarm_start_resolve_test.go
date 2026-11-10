package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedBuffer — приёмник журнала, общий для горутин сервера.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureServerLogs направляет вывод slog и log по умолчанию в буфер до
// конца теста.
func captureServerLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	logs := &lockedBuffer{}
	previous, writer, flags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() {
		// SetDefault перенаправил и пакет log: возвращаем его вывод отдельно.
		slog.SetDefault(previous)
		log.SetOutput(writer)
		log.SetFlags(flags)
	})
	return logs
}

// resolveStartSetup — startAPISetup, у проекта которого repository
// gitTestOrigin с серверным Git-доступом: его задаёт access (nil — проектный
// Git credential "gitlab"), а TLS upstream git.example.com отвечает inner на
// запросы с этим credential.
func resolveStartSetup(t *testing.T, access func(startAPIFixture), inner http.Handler) (startAPIFixture, *gitUpstream) {
	t.Helper()
	f := startAPISetup(t)
	if err := f.ts.srv.store.SetProjectRepository(context.Background(), f.project, gitTestOrigin); err != nil {
		t.Fatal(err)
	}
	if access == nil {
		access = func(f startAPIFixture) {
			putProjectGitCredential(t, f.ts, f.project, "gitlab", gitTestOrigin, gitTokenSentinel)
			if err := f.ts.srv.store.SetProjectGitCredentialRef(context.Background(), f.project, "gitlab"); err != nil {
				t.Fatal(err)
			}
		}
	}
	access(f)
	return f, newGitUpstream(t, f.ts, inner)
}

// pinnedSource — снимок источника роя, "" до первого запуска.
func (f startAPIFixture) pinnedSource(t *testing.T) string {
	t.Helper()
	sw, err := f.ts.srv.store.GetSwarm(context.Background(), f.swarm)
	if err != nil {
		t.Fatal(err)
	}
	if sw.Source == nil {
		return ""
	}
	return sw.Source.Origin + " " + sw.Source.Commit
}

func (f startAPIFixture) startRaw(t *testing.T, body any) (*http.Response, map[string]any, string) {
	t.Helper()
	return f.ts.do(f.alice, "POST", "/api/swarms/"+f.swarm+"/start", f.ts.csrf(f.alice), body)
}

// FEATURE: Сервер разрешает ветку проекта в commit при запуске роя (#228).
// SPEC: Первый запуск без commit пиннит commit, который в этот момент
// называет HEAD repository проекта: один запрос объявления ссылок
// (info/refs?service=git-upload-pack) через Git-доступ шлюза — проектный
// credential или remote BROOD_GIT_REMOTES — до транзакции записи. Снимок
// источника — канонический repository и commit; credential нет ни в ответе,
// ни в журнале, ни в снимке. Повторный запуск идёт на закреплённом источнике
// и ничего не разрешает, даже когда ветка ушла вперёд.
// IMPLEMENTATION: git http-backend по TLS за транспортом шлюза.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestSwarmStartResolvesProjectBranch(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	for _, tc := range []struct {
		name   string
		access func(*testing.T) func(startAPIFixture)
	}{
		{"project credential", func(*testing.T) func(startAPIFixture) { return nil }},
		{"installation remote", func(t *testing.T) func(startAPIFixture) {
			return func(f startAPIFixture) {
				f.ts.srv.git.remotes = gitRemotes{gitTestOrigin: {username: "brood-bot", token: gitTokenSentinel}}
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureServerLogs(t)
			home := t.TempDir()
			_, bare, backend := seededGitBackend(t, gitPath, home)
			// HEAD называет trunk на commit впереди main: сервер идёт за HEAD,
			// а не за привычным именем ветки.
			tree := strings.TrimSpace(mustGit(t, home, bare, "rev-parse", "main^{tree}"))
			trunk := strings.TrimSpace(mustGit(t, home, bare, "commit-tree", "-p", "main", "-m", "trunk", tree))
			mustGit(t, home, bare, "update-ref", "refs/heads/trunk", trunk)
			mustGit(t, home, bare, "symbolic-ref", "HEAD", "refs/heads/trunk")
			if main := strings.TrimSpace(mustGit(t, home, bare, "rev-parse", "main")); main == trunk {
				t.Fatal("trunk equals main")
			}

			f, upstream := resolveStartSetup(t, tc.access(t), backend)
			life := f.lifeState(t)
			r, b, raw := f.startRaw(t, map[string]any{"definition": "release"})
			expectStatus(t, "start without commit", r, b, http.StatusOK)
			source, _ := b["source"].(map[string]any)
			if source["origin"] != gitTestOrigin || source["commit"] != trunk || b["execution"] != "waiting" {
				t.Fatalf("start response: %s", raw)
			}
			if got := f.pinnedSource(t); got != gitTestOrigin+" "+trunk {
				t.Fatalf("source snapshot %q", got)
			}
			if got := f.lifeState(t); got != life {
				t.Fatalf("start touched actors:\n%s\nwant\n%s", got, life)
			}
			seen := upstream.seen()
			if len(seen) != 1 || seen[0].method != http.MethodGet || seen[0].path != "/group/repo.git/info/refs" || seen[0].query != "service=git-upload-pack" {
				t.Fatalf("upstream requests: %+v", seen)
			}
			header := seen[0].header
			if user, password, ok := (&http.Request{Header: header}).BasicAuth(); !ok || user != "brood-bot" || password != gitTokenSentinel {
				t.Fatal("upstream did not receive the project Git access credential")
			}
			if header.Get("User-Agent") != gitGatewayUserAgent || header.Get("Git-Protocol") != "" || header.Get("Cookie") != "" {
				t.Fatalf("upstream request headers: %v", header.Get("User-Agent"))
			}

			// Ветка ушла вперёд; повторный запуск держит закреплённый commit и никого не спрашивает.
			moved := strings.TrimSpace(mustGit(t, home, bare, "commit-tree", "-p", trunk, "-m", "moved", tree))
			mustGit(t, home, bare, "update-ref", "refs/heads/trunk", moved)
			r, b = f.stop(f.alice)
			expectStatus(t, "stop", r, b, http.StatusOK)
			r, b, raw = f.startRaw(t, map[string]any{"definition": "release"})
			expectStatus(t, "restart", r, b, http.StatusOK)
			if b["source"].(map[string]any)["commit"] != trunk || b["step"].(map[string]any)["sequence"] != float64(2) {
				t.Fatalf("restart response: %s", raw)
			}
			if len(upstream.seen()) != 1 {
				t.Fatalf("restart resolved the branch: %d upstream requests", len(upstream.seen()))
			}
			for _, secret := range []string{gitTokenSentinel, "brood-bot"} {
				if strings.Contains(raw, secret) || strings.Contains(f.pinnedSource(t), secret) || strings.Contains(logs.String(), secret) {
					t.Fatalf("credential %q disclosed", secret)
				}
			}
		})
	}
}

// SPEC: Явный commit в теле запуска пиннится как прежде, без запроса к
// repository.
func TestSwarmStartNamedCommitSkipsResolution(t *testing.T) {
	f, upstream := resolveStartSetup(t, nil, gitAdvertisement())
	r, b, raw := f.startRaw(t, map[string]any{"definition": "release", "commit": startAPICommit})
	expectStatus(t, "start with commit", r, b, http.StatusOK)
	if got := f.pinnedSource(t); got != gitTestOrigin+" "+startAPICommit {
		t.Fatalf("source snapshot %q: %s", got, raw)
	}
	if seen := upstream.seen(); len(seen) != 0 {
		t.Fatalf("named commit reached upstream: %+v", seen)
	}
}

// advertise отвечает телом объявления upload-pack.
func advertise(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		_, _ = io.WriteString(w, body)
	})
}

func answer(status int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream says "+gitTokenSentinel, status)
	})
}

// SPEC: Первый запуск, ветку которого разрешить нельзя, отвергается с
// причиной до любой записи: нет доступа к repository (запроса нет), сбой
// сети, адрес, который шлюз отвергает при dial, 401/403, 404, прочие статусы,
// редирект (не выполняется), ответ не smart HTTP, повреждённое или слишком
// большое объявление, repository без HEAD, HEAD не на ветке, SHA-256 и
// таймаут. Причина — фиксированный текст: ни адрес upstream, ни его тело, ни
// credential не попадают в ответ и журнал, база не меняется.
func TestSwarmStartResolutionRefusals(t *testing.T) {
	head := headTestCommit + " HEAD"
	header := pktLine("# service=git-upload-pack\n") + "0000"
	for _, tc := range []struct {
		name     string
		access   func(*testing.T) func(startAPIFixture)
		inner    http.Handler
		prepare  func(*testing.T, startAPIFixture, *gitUpstream)
		status   int
		reason   string
		requests int
	}{
		{name: "no server Git access", access: func(*testing.T) func(startAPIFixture) { return func(startAPIFixture) {} }, status: http.StatusUnprocessableEntity, reason: "has no server Git access", requests: 0},
		{name: "credential bound to another repository", access: func(t *testing.T) func(startAPIFixture) {
			return func(f startAPIFixture) {
				putProjectGitCredential(t, f.ts, f.project, "gitlab", "https://git.example.com/group/other.git", gitTokenSentinel)
				if err := f.ts.srv.store.SetProjectGitCredentialRef(context.Background(), f.project, "gitlab"); err != nil {
					t.Fatal(err)
				}
			}
		}, status: http.StatusUnprocessableEntity, reason: "has no server Git access", requests: 0},
		{name: "upstream unreachable", prepare: func(t *testing.T, f startAPIFixture, _ *gitUpstream) {
			f.ts.srv.git.transport.DialContext = func(context.Context, string, string) (net.Conn, error) {
				return nil, errors.New("dial git.example.com: connection refused " + gitTokenSentinel)
			}
		}, status: http.StatusBadGateway, reason: "Git upstream request failed", requests: 0},
		{name: "address refused at dial", prepare: func(t *testing.T, f startAPIFixture, u *gitUpstream) {
			loopback := u.server.Listener.Addr().String()
			f.ts.srv.git.transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{Control: gitDialControl}).DialContext(ctx, network, loopback)
			}
		}, status: http.StatusBadGateway, reason: "Git upstream address denied", requests: 0},
		{name: "credential rejected", prepare: func(_ *testing.T, _ startAPIFixture, u *gitUpstream) { u.rotate("rotated-upstream-token") }, status: http.StatusBadGateway, reason: "Git upstream denied access", requests: 1},
		{name: "forbidden", inner: answer(http.StatusForbidden), status: http.StatusBadGateway, reason: "Git upstream denied access", requests: 1},
		{name: "repository not found", inner: answer(http.StatusNotFound), status: http.StatusBadGateway, reason: "Git upstream repository not found", requests: 1},
		{name: "server error", inner: answer(http.StatusInternalServerError), status: http.StatusBadGateway, reason: "Git upstream request failed", requests: 1},
		{name: "redirect", inner: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/group/elsewhere.git/info/refs?service=git-upload-pack", http.StatusFound)
		}), status: http.StatusBadGateway, reason: "Git upstream redirect denied", requests: 1},
		{name: "dumb protocol", inner: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, headTestCommit+"\trefs/heads/trunk\n")
		}), status: http.StatusBadGateway, reason: "malformed Git ref advertisement", requests: 1},
		{name: "malformed advertisement", inner: advertise("zzzz" + gitTokenSentinel), status: http.StatusBadGateway, reason: "malformed Git ref advertisement", requests: 1},
		{name: "too large advertisement", inner: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			_, _ = io.WriteString(w, pktLine("# service=git-upload-pack\n"))
			metadata := pktLine(strings.Repeat("m", 996))
			for range 1 << 12 {
				if _, err := io.WriteString(w, metadata); err != nil || r.Context().Err() != nil {
					return
				}
			}
		}), status: http.StatusBadGateway, reason: "Git ref advertisement too large", requests: 1},
		{name: "empty repository", inner: advertise(header + pktLine(strings.Repeat("0", 40)+" capabilities^{}\x00"+headTestCaps+"\n") + "0000"), status: http.StatusUnprocessableEntity, reason: "the repository has no default branch", requests: 1},
		{name: "detached HEAD", inner: advertise(header + pktLine(head+"\x00multi_ack agent=git/2.43.0\n") + "0000"), status: http.StatusUnprocessableEntity, reason: "the repository HEAD is not a branch", requests: 1},
		{name: "SHA-256 repository", inner: advertise(header + pktLine(strings.Repeat("ab", 32)+" HEAD\x00symref=HEAD:refs/heads/trunk object-format=sha256\n") + "0000"), status: http.StatusUnprocessableEntity, reason: "not a SHA-1 commit", requests: 1},
		{name: "timeout", inner: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
		}), prepare: func(_ *testing.T, f startAPIFixture, _ *gitUpstream) {
			f.ts.srv.git.headTimeout = 200 * time.Millisecond
		}, status: http.StatusGatewayTimeout, reason: "Git upstream request timed out", requests: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureServerLogs(t)
			var access func(startAPIFixture)
			if tc.access != nil {
				access = tc.access(t)
			}
			inner := tc.inner
			if inner == nil {
				inner = advertise(headAdvertisement(head, headTestCaps))
			}
			f, upstream := resolveStartSetup(t, access, inner)
			if tc.prepare != nil {
				tc.prepare(t, f, upstream)
			}
			before, life := f.executionState(t), f.lifeState(t)
			r, b, raw := f.startRaw(t, map[string]any{"definition": "release"})
			expectStatus(t, tc.name, r, b, tc.status)
			if detail, _ := b["detail"].(string); !strings.HasPrefix(detail, "cannot resolve the project branch: ") || !strings.Contains(detail, tc.reason) {
				t.Fatalf("reason %q does not name %q", detail, tc.reason)
			}
			if got := f.executionState(t); got != before {
				t.Fatalf("refused start wrote:\n%s\nwant\n%s", got, before)
			}
			if got := f.lifeState(t); got != life {
				t.Fatalf("refused start touched actors:\n%s\nwant\n%s", got, life)
			}
			if got := f.pinnedSource(t); got != "" {
				t.Fatalf("refused start pinned %q", got)
			}
			if got := len(upstream.seen()); got != tc.requests {
				t.Fatalf("upstream requests = %d, want %d", got, tc.requests)
			}
			for _, secret := range []string{gitTokenSentinel, "brood-bot", "git.example.com", "rotated-upstream-token"} {
				if strings.Contains(raw, secret) || strings.Contains(logs.String(), secret) {
					t.Fatalf("refusal disclosed %q: %s", secret, raw)
				}
			}
		})
	}
}

// SPEC: Тому, кто не управляет роем, сервер ничего не разрешает: запуск
// зрителя без commit отвергается как прежде, без запроса с credential проекта.
func TestSwarmStartViewerResolvesNothing(t *testing.T) {
	f, upstream := resolveStartSetup(t, nil, advertise(headAdvertisement(headTestCommit+" HEAD", headTestCaps)))
	r, b, _ := f.ts.do(f.alice, "POST", "/api/swarms/"+f.swarm+"/members", f.ts.csrf(f.alice), map[string]string{"username": "bob", "role": "viewer"})
	expectStatus(t, "add viewer", r, b, http.StatusNoContent)
	before := f.executionState(t)
	r, b = f.start(f.bob, map[string]any{"definition": "release"})
	expectStatus(t, "viewer start", r, b, http.StatusForbidden)
	if got := f.executionState(t); got != before || len(upstream.seen()) != 0 {
		t.Fatalf("viewer start: %d upstream requests, state\n%s\nwant\n%s", len(upstream.seen()), got, before)
	}
}

// SPEC: Repository проекта, сменившийся, пока сервер разрешал его ветку,
// отвергает запуск в транзакции записи (409): снимок не собирается из commit
// одного repository и адреса другого.
func TestSwarmStartRepositoryChangedDuringResolution(t *testing.T) {
	const other = "https://git.example.com/group/other.git"
	f, upstream := resolveStartSetup(t, nil, nil)
	upstream.setInner(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := f.ts.srv.store.SetProjectRepository(r.Context(), f.project, other); err != nil {
			http.Error(w, "fixture", http.StatusInternalServerError)
			return
		}
		advertise(headAdvertisement(headTestCommit+" HEAD", headTestCaps)).ServeHTTP(w, r)
	}))
	before := f.executionState(t)
	r, b, _ := f.startRaw(t, map[string]any{"definition": "release"})
	expectStatus(t, "start", r, b, http.StatusConflict)
	if detail, _ := b["detail"].(string); !strings.Contains(detail, "repository changed") {
		t.Fatalf("reason %q", detail)
	}
	if got := f.executionState(t); got != before || f.pinnedSource(t) != "" || len(upstream.seen()) != 1 {
		t.Fatalf("refused start wrote:\n%s\nwant\n%s", got, before)
	}
}
