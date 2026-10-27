// Проверки запускают только собственные процессы с приватными данными на loopback.
package acceptance

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type binaries struct{ brood, bro string }

func buildBinaries(t *testing.T) binaries {
	t.Helper()
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", dir+string(os.PathSeparator), "./cmd/brood", "./cmd/bro")
	cmd.Dir = ".."
	// Не скачиваем зависимости и не наследуем настройки живой установки.
	cmd.Env = append(compilerEnv(), "CGO_ENABLED=0", "GOROOT="+runtime.GOROOT(), "GOENV=off", "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build real binaries: %v\n%s", err, out)
	}
	return binaries{filepath.Join(dir, "brood"), filepath.Join(dir, "bro")}
}

func compilerEnv() []string {
	var env []string
	for _, name := range []string{"PATH", "HOME", "TMPDIR", "GOCACHE", "GOMODCACHE", "GOFLAGS", "GOWORK", "GOPATH", "GOROOT"} {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}

type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

type child struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
	log  safeBuffer
}

type installation struct {
	t           *testing.T
	bin         binaries
	dir, origin string
	env         []string
	process     *child
	passwords   []string
	tokens      []string
	outputs     []string
}

func newInstallation(t *testing.T, bin binaries, extra ...string) *installation {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		t.Fatal(err)
	}
	secret := hex.EncodeToString(secretBytes)
	db := filepath.Join(dir, "brood.db")
	// Файл и каталог принадлежат только этому тесту, включая повторный запуск.
	if err := os.WriteFile(db, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f := &installation{t: t, bin: bin, dir: dir, origin: "http://" + addr, tokens: []string{secret}}
	f.env = []string{
		"HOME=" + dir, "XDG_STATE_HOME=" + filepath.Join(dir, "state"),
		"BROOD_DB_PATH=" + db, "BROOD_ADDR=" + addr,
		"BROOD_SESSION_SECRET=" + secret, "BROOD_SECURE_COOKIES=false",
	}
	f.env = append(f.env, extra...)
	t.Cleanup(func() {
		f.stop()
		f.auditFiles()
		for _, output := range f.outputs {
			f.assertNoSecrets(output)
		}
	})
	f.passwords = append(f.passwords, "admin-acceptance-password-4821")
	f.run(bin.brood, "admin\n"+f.passwords[0]+"\n", "", 0, "bootstrap-admin")
	f.start()
	return f
}

func (f *installation) start() {
	f.t.Helper()
	if f.process != nil {
		f.t.Fatal("attempted to start a second server without stopping the first")
	}
	p := &child{done: make(chan struct{})}
	p.cmd = exec.Command(f.bin.brood, "serve")
	p.cmd.Dir, p.cmd.Env = f.dir, f.env
	p.cmd.Stdout, p.cmd.Stderr = &p.log, &p.log
	if err := p.cmd.Start(); err != nil {
		f.t.Fatalf("start brood serve: %v", err)
	}
	f.process = p
	go func() { p.err = p.cmd.Wait(); close(p.done) }()
	client := &http.Client{Timeout: 200 * time.Millisecond, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-p.done:
			f.t.Fatalf("brood serve exited before readiness: %v; %s", p.err, f.redact(p.log.String()))
		case <-deadline.C:
			f.t.Fatalf("brood serve not ready within 8s: %s", f.redact(p.log.String()))
		case <-tick.C:
			resp, err := client.Get(f.origin + "/healthz")
			if err != nil {
				continue
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				select {
				case <-p.done:
					f.t.Fatal("brood exited while becoming ready")
				default:
					return
				}
			}
		}
	}
}

func (f *installation) stop() {
	f.t.Helper()
	p := f.process
	if p == nil {
		return
	}
	f.process = nil
	// Только точный дочерний процесс: никаких поиска PID или process groups.
	select {
	case <-p.done:
		f.t.Errorf("brood serve exited unexpectedly: %v", p.err)
	default:
		if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			f.t.Errorf("signal owned server: %v", err)
		}
		select {
		case <-p.done:
			if p.err != nil {
				f.t.Errorf("graceful server shutdown: %v", p.err)
			}
		case <-time.After(3 * time.Second):
			_ = p.cmd.Process.Kill()
			<-p.done
			f.t.Error("owned server required kill after shutdown timeout")
		}
	}
	f.outputs = append(f.outputs, p.log.String())
}

func (f *installation) run(binary, input, state string, want int, args ...string) string {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir, cmd.Env = f.dir, append([]string(nil), f.env...)
	if state != "" {
		cmd.Env = append(cmd.Env, "XDG_STATE_HOME="+filepath.Join(f.dir, state))
	}
	if lines := strings.Split(input, "\n"); len(lines) > 1 && lines[1] != "" {
		f.passwords = append(f.passwords, lines[1])
	}
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			f.t.Fatalf("run %s: %v", filepath.Base(binary), err)
		}
		code = exit.ExitCode()
	}
	f.outputs = append(f.outputs, string(out))
	f.auditFiles()
	f.assertNoSecrets(string(out))
	if ctx.Err() != nil {
		f.t.Fatalf("%s %v exceeded 5s", filepath.Base(binary), args)
	}
	if code != want {
		f.t.Errorf("%s %v exit %d, want %d: %s", filepath.Base(binary), args, code, want, f.redact(string(out)))
	}
	return string(out)
}

func (f *installation) cli(state, input string, want int, args ...string) string {
	f.t.Helper()
	return f.run(f.bin.bro, input, state, want, args...)
}

func (f *installation) redact(value string) string {
	for _, secret := range append(append([]string(nil), f.passwords...), f.tokens...) {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[redacted]")
		}
	}
	return value
}

func (f *installation) assertNoSecrets(value string) {
	f.t.Helper()
	if f.redact(value) != value {
		f.t.Error("password or session secret appeared in process output or HTTP response")
	}
	if strings.Contains(value, "panic:") || strings.Contains(value, "goroutine ") {
		f.t.Error("process output or HTTP response contains a Go panic/traceback")
	}
}

func (f *installation) auditFiles() {
	f.t.Helper()
	err := filepath.WalkDir(f.dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, password := range f.passwords {
			if bytes.Contains(data, []byte(password)) {
				f.t.Errorf("plaintext password persisted in %s", path)
			}
		}
		protected := strings.Contains(path, string(os.PathSeparator)+"brood"+string(os.PathSeparator)+"sessions"+string(os.PathSeparator)) || strings.HasPrefix(path, filepath.Join(f.dir, "brood.db"))
		if protected {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Mode().Perm() != 0o600 {
				f.t.Errorf("protected state %s mode %o, want 600", path, info.Mode().Perm())
			}
		}
		if protected && strings.HasSuffix(path, ".json") {
			var saved struct{ Session, CSRF string }
			if err := json.Unmarshal(data, &saved); err != nil {
				return fmt.Errorf("decode saved CLI state: %w", err)
			}
			for _, token := range []string{saved.Session, saved.CSRF} {
				if token != "" {
					f.tokens = append(f.tokens, token)
				}
			}
		}
		if !protected {
			for _, token := range f.tokens {
				if token != "" && bytes.Contains(data, []byte(token)) {
					f.t.Errorf("session secret persisted outside protected state: %s", path)
				}
			}
		}
		return nil
	})
	if err != nil {
		f.t.Errorf("audit private state: %v", err)
	}
}

type browser struct {
	f      *installation
	client *http.Client
	base   *url.URL
}

func (f *installation) browser() *browser {
	f.t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		f.t.Fatal(err)
	}
	base, err := url.Parse(f.origin)
	if err != nil {
		f.t.Fatal(err)
	}
	transport := &http.Transport{Proxy: nil}
	f.t.Cleanup(transport.CloseIdleConnections)
	return &browser{f, &http.Client{Jar: jar, Timeout: 3 * time.Second, Transport: transport}, base}
}

func (b *browser) request(method, path string, body any, headers map[string]string) (int, []byte, http.Header) {
	b.f.t.Helper()
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			b.f.t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, b.f.origin+path, bytes.NewReader(data))
	if err != nil {
		b.f.t.Fatal(err)
	}
	if method != http.MethodGet {
		var fields map[string]any
		if json.Unmarshal(data, &fields) == nil {
			for _, key := range []string{"password", "new_password", "current_password"} {
				if value, ok := fields[key].(string); ok && value != "" {
					b.f.passwords = append(b.f.passwords, value)
				}
			}
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", b.f.origin)
		for _, c := range b.client.Jar.Cookies(b.base) {
			if c.Name == "csrf" {
				req.Header.Set("X-CSRFToken", c.Value)
			}
		}
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		b.f.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		b.f.t.Fatal(err)
	}
	for _, c := range b.client.Jar.Cookies(b.base) {
		if c.Name == "session" || c.Name == "csrf" {
			b.f.tokens = append(b.f.tokens, c.Value)
		}
	}
	b.f.assertNoSecrets(string(data))
	return resp.StatusCode, data, resp.Header
}

func (b *browser) expect(method, path string, body any, want int) []byte {
	b.f.t.Helper()
	code, data, header := b.request(method, path, body, nil)
	if code != want {
		b.f.t.Fatalf("%s %s: status %d, want %d: %s", method, path, code, want, b.f.redact(string(data)))
	}
	if len(data) > 0 && !strings.Contains(header.Get("Content-Type"), "application/json") {
		b.f.t.Fatalf("%s %s: expected JSON response", method, path)
	}
	return data
}

func (b *browser) login(username, password string) {
	b.f.t.Helper()
	b.f.passwords = append(b.f.passwords, password)
	b.expect("GET", "/api/auth/session", nil, 200)
	_, old := b.cookies()
	b.expect("POST", "/api/auth/login", map[string]string{"username": username, "password": password}, 204)
	session, csrf := b.cookies()
	if session == "" || csrf == "" || csrf == old {
		b.f.t.Fatal("login must issue a session and rotate CSRF")
	}
	var info struct {
		Authenticated bool
		User          struct{ Username string }
	}
	decode(b.f.t, b.expect("GET", "/api/auth/session", nil, 200), &info)
	if !info.Authenticated || info.User.Username != username {
		b.f.t.Fatal("login did not authenticate the requested user")
	}
}

func (b *browser) cookies() (session, csrf string) {
	for _, c := range b.client.Jar.Cookies(b.base) {
		switch c.Name {
		case "session":
			session = c.Value
		case "csrf":
			csrf = c.Value
		}
	}
	return
}

func decode(t *testing.T, data []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("decode API response: %v", err)
	}
}
