//go:build linux

package guest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Policy is exact-host only; a single unsafe address poisons the whole answer.
func TestPolicy(t *testing.T) {
	for _, host := range []string{"", "*.example.com", "example.com/path", "user@example.com", "localhost", "127.0.0.1", "[::1]", "2130706433", "foo.123", "example.com\r\nX: a", "a..com", "-a.com", "é.com"} {
		if _, err := NewPolicy([]string{host}, publicLookup); err == nil {
			t.Errorf("accepted host %q", host)
		}
	}
	if _, err := NewPolicy(nil, publicLookup); err == nil {
		t.Fatal("empty policy")
	}
	if _, err := NewPolicy([]string{"example.com"}, nil); err == nil {
		t.Fatal("missing resolver")
	}
	p, err := NewPolicy([]string{"Example.COM."}, publicLookup)
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"example.com", "EXAMPLE.COM."} {
		if _, err := p.Resolve(context.Background(), host); err != nil {
			t.Fatal(err)
		}
	}
	for _, host := range []string{"sub.example.com", "example.com.evil.org", "example.com:443", "example.com\x00", "example.com..", "0x7f.0.0.1"} {
		if _, err := p.Resolve(context.Background(), host); err == nil {
			t.Errorf("allowed %q", host)
		}
	}
	for _, ip := range []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "0.0.0.0", "224.1.1.1", "255.255.255.255", "100.64.0.1", "192.0.0.1", "198.18.0.1", "::", "::1", "fc00::1", "fe80::1", "ff02::1", "::ffff:127.0.0.1", "64:ff9b::7f00:1"} {
		t.Run(ip, func(t *testing.T) {
			p, _ := NewPolicy([]string{"example.com"}, func(context.Context, string) ([]net.IP, error) {
				return []net.IP{net.ParseIP("8.8.8.8"), net.ParseIP(ip)}, nil
			})
			if _, err := p.Resolve(context.Background(), "example.com"); err == nil {
				t.Fatal("mixed unsafe answer accepted")
			}
		})
	}
	for _, ips := range [][]net.IP{nil, {nil}, {net.ParseIP("2606:4700:4700::1111")}} {
		p, _ := NewPolicy([]string{"example.com"}, func(context.Context, string) ([]net.IP, error) { return ips, nil })
		_, err := p.Resolve(context.Background(), "example.com")
		if (err == nil) != (len(ips) == 1 && ips[0] != nil) {
			t.Fatalf("answer %v: %v", ips, err)
		}
	}
}
func publicLookup(context.Context, string) ([]net.IP, error) {
	return []net.IP{net.ParseIP("8.8.8.8")}, nil
}

func TestProxyRejectsAndPinsDial(t *testing.T) {
	var mu sync.Mutex
	lookups := 0
	var targets []string
	p, _ := NewPolicy([]string{"example.com"}, func(context.Context, string) ([]net.IP, error) {
		mu.Lock()
		defer mu.Unlock()
		lookups++
		if lookups > 1 {
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		}
		return publicLookup(context.Background(), "")
	})
	proxy, err := NewProxy(p, func(_ context.Context, network, address string) (net.Conn, error) {
		mu.Lock()
		targets = append(targets, address)
		mu.Unlock()
		a, b := net.Pipe()
		go func() {
			defer b.Close()
			buf := make([]byte, 4096)
			_, _ = b.Read(buf)
			_, _ = io.WriteString(b, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
		}()
		return a, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "http://example.com/test", nil)
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Body.String() != "ok" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("rebinding: %d", rec.Code)
	}
	mu.Lock()
	if !reflect.DeepEqual(targets, []string{"8.8.8.8:80"}) {
		t.Fatal(targets)
	}
	mu.Unlock()
	p, _ = NewPolicy([]string{"example.com"}, publicLookup)
	proxy, _ = NewProxy(p, func(context.Context, string, string) (net.Conn, error) {
		t.Error("denied request dialed")
		return nil, errors.New("no")
	})
	for _, target := range []string{"https://example.com/", "ftp://example.com/", "http://evil.com/", "http://example.com:22/", "http://u:p@example.com/", "http://127.0.0.1/", "http://[::1]/", "http://example.com/%0d%0aInjected:x"} {
		rec := httptest.NewRecorder()
		proxy.ServeHTTP(rec, httptest.NewRequest("GET", target, nil))
		if rec.Code != 403 {
			t.Errorf("%s: %d", target, rec.Code)
		}
	}
	req = httptest.NewRequest("GET", "http://example.com/", nil)
	req.Header.Set("X-Test", "bad\r\nheader")
	rec = httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatal("header control accepted")
	}
	for _, target := range []string{"evil.com:443", "example.com:22", "example.com", "user@example.com:443", "[::1]:443"} {
		req := httptest.NewRequest("CONNECT", "http://unused", nil)
		req.URL.Host = target
		req.Host = target
		rec := httptest.NewRecorder()
		proxy.ServeHTTP(rec, req)
		if rec.Code != 403 {
			t.Errorf("CONNECT %s: %d", target, rec.Code)
		}
	}
	if _, err := NewProxy(nil, nil); err == nil {
		t.Fatal("missing dependencies")
	}
}

func TestConnectTunnel(t *testing.T) {
	p, _ := NewPolicy([]string{"example.com"}, publicLookup)
	proxy, _ := NewProxy(p, func(_ context.Context, _, address string) (net.Conn, error) {
		if address != "8.8.8.8:443" {
			t.Errorf("dial %s", address)
		}
		a, b := net.Pipe()
		go func() { defer b.Close(); _, _ = io.Copy(b, b) }()
		return a, nil
	})
	server := httptest.NewServer(proxy)
	defer server.Close()
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(server.URL, "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	_, _ = io.WriteString(conn, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
	buf := make([]byte, len("HTTP/1.1 200 Connection Established\r\n\r\n"))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(buf), "HTTP/1.1 200 ") {
		t.Fatal(string(buf))
	}
	_, _ = io.WriteString(conn, "ping")
	buf = make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("%q %v", buf, err)
	}
}

func dnsQuery(t *testing.T, host string, typ dnsmessage.Type) []byte {
	t.Helper()
	m := dnsmessage.Message{Header: dnsmessage.Header{ID: 42, RecursionDesired: true}, Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName(host), Type: typ, Class: dnsmessage.ClassINET}}}
	b, err := m.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func TestDNS(t *testing.T) {
	p, _ := NewPolicy([]string{"example.com"}, func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("8.8.8.8"), net.ParseIP("2606:4700:4700::1111")}, nil
	})
	for _, tc := range []struct {
		host    string
		typ     dnsmessage.Type
		allowed bool
	}{{"example.com.", dnsmessage.TypeA, true}, {"example.com.", dnsmessage.TypeAAAA, true}, {"example.com.", dnsmessage.TypeTXT, false}, {"unknown.com.", dnsmessage.TypeA, false}, {"example.com.", dnsmessage.TypeMX, false}} {
		reply, err := p.DNSReply(context.Background(), dnsQuery(t, tc.host, tc.typ))
		if err != nil {
			t.Fatal(err)
		}
		var m dnsmessage.Message
		if err = m.Unpack(reply); err != nil {
			t.Fatal(err)
		}
		if m.ID != 42 || !m.Response {
			t.Fatal(m.Header)
		}
		if tc.allowed {
			if len(m.Answers) != 1 || m.RCode != dnsmessage.RCodeSuccess {
				t.Fatal(m)
			}
		} else if len(m.Answers) != 0 || m.RCode != dnsmessage.RCodeRefused {
			t.Fatal(m)
		}
	}
	for _, bad := range [][]byte{nil, {0, 1, 2}, {255, 255, 255, 255, 255, 255}} {
		if _, err := p.DNSReply(context.Background(), bad); err == nil {
			t.Fatal("malformed DNS accepted")
		}
	}
	p, _ = NewPolicy([]string{"example.com"}, func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("8.8.8.8"), net.ParseIP("::1")}, nil
	})
	reply, _ := p.DNSReply(context.Background(), dnsQuery(t, "example.com.", dnsmessage.TypeA))
	var m dnsmessage.Message
	_ = m.Unpack(reply)
	if m.RCode != dnsmessage.RCodeRefused || len(m.Answers) != 0 {
		t.Fatal("mixed DNS answer leaked")
	}
}

func TestActorEnvironmentAndPayload(t *testing.T) {
	t.Setenv("BROOD_GUEST_TOKEN", "private-token")
	t.Setenv("ROOT_SECRET", "private-root")
	t.Setenv("ANTHROPIC_API_KEY", "private-key")
	t.Setenv("PATH", "/evil")
	spec := actorSpec("/trusted/guest")
	joined := strings.Join(spec.Env, "\n")
	for _, forbidden := range []string{"BROOD_GUEST_", "private-", "/evil", "ANTHROPIC_API_KEY"} {
		if strings.Contains(joined, forbidden) {
			t.Fatal(joined)
		}
	}
	for _, want := range []string{"HOME=/workspace/home", "PWD=/workspace/project", "HTTP_PROXY=http://127.0.0.1:3128", "HTTPS_PROXY=http://127.0.0.1:3128"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %s: %s", want, joined)
		}
	}
	if spec.UID != 1000 || spec.GID != 1000 || spec.Dir != "/workspace/project" || !reflect.DeepEqual(spec.Args, []string{"--brood-guest-actor"}) {
		t.Fatalf("%+v", spec)
	}
	if !reflect.DeepEqual(actorArgs(), []string{"--interface", "127.0.0.1", "--port", "8080", "--writable", "--check-origin", "tmux", "new-session", "-A", "-s", "claude", "claude", "--mcp-config", `{"mcpServers":{"brood":{"command":"/usr/local/bin/brood-guest","args":["mcp"]}}}`}) {
		t.Fatal(actorArgs())
	}
}

func privateTemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestWorkspaceEscape(t *testing.T) {
	for _, name := range []string{"home", "project"} {
		t.Run(name, func(t *testing.T) {
			root := privateTemp(t)
			outside := t.TempDir()
			if err := os.Symlink(outside, filepath.Join(root, name)); err != nil {
				t.Fatal(err)
			}
			if err := prepareWorkspace(root, os.Getuid(), os.Getgid()); err == nil {
				t.Fatal("workspace symlink accepted")
			}
			items, _ := os.ReadDir(outside)
			if len(items) != 0 {
				t.Fatal("outside modified")
			}
		})
	}
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "workspace")
	_ = os.Symlink(real, link)
	if err := prepareWorkspace(link, os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("workspace root symlink accepted")
	}
	root := privateTemp(t)
	if err := prepareWorkspace(root, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "home", "keep")
	_ = os.WriteFile(marker, []byte("persistent"), 0600)
	if err := prepareWorkspace(root, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(marker)
	if string(b) != "persistent" {
		t.Fatal("persistent data lost")
	}
}

func TestNoNewPrivilegesSubprocess(t *testing.T) {
	if os.Getenv("GUEST_PRIVILEGE_TEST") == "1" {
		if err := lockPrivileges(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		status, _ := os.ReadFile("/proc/self/status")
		for _, want := range []string{"NoNewPrivs:\t1", "CapEff:\t0000000000000000", "CapAmb:\t0000000000000000"} {
			if !bytes.Contains(status, []byte(want)) {
				os.Exit(3)
			}
		}
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestNoNewPrivilegesSubprocess$")
	cmd.Env = append(os.Environ(), "GUEST_PRIVILEGE_TEST=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
}

type fakeProcess struct {
	done   chan struct{}
	once   sync.Once
	events *[]string
}

func (p *fakeProcess) Wait() error { <-p.done; return nil }
func (p *fakeProcess) Close() error {
	p.once.Do(func() { *p.events = append(*p.events, "process-close"); close(p.done) })
	return nil
}

type fakeServices struct {
	done   chan error
	events *[]string
}

func (s *fakeServices) Done() <-chan error { return s.done }
func (s *fakeServices) Close() error       { *s.events = append(*s.events, "services-close"); return nil }
func testConfig(events *[]string) (Config, *fakeServices) {
	s := &fakeServices{make(chan error, 1), events}
	return Config{Hosts: []string{"example.com"}, Executable: "/trusted/guest", Dependencies: &Dependencies{EUID: func() int { return 0 }, Lookup: publicLookup, Workspace: func() error { *events = append(*events, "workspace"); return nil }, Rules: func(_ context.Context, script string) error {
		if strings.HasPrefix(script, "delete") {
			*events = append(*events, "rules-delete")
		} else {
			*events = append(*events, "rules-install")
		}
		return nil
	}, Services: func(context.Context, *Policy) (Services, error) {
		*events = append(*events, "services-start")
		return s, nil
	}, Launch: func(context.Context, ActorSpec) (Process, error) {
		*events = append(*events, "process-start")
		return &fakeProcess{done: make(chan struct{}), events: events}, nil
	}}}, s
}
func TestLifecycle(t *testing.T) {
	var events []string
	c, s := testConfig(&events)
	h, err := Start(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if h.TerminalDialTarget() != "127.0.0.1:8080" {
		t.Fatal(h.TerminalDialTarget())
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.Wait(); err != nil {
		t.Fatal(err)
	}
	want := []string{"workspace", "rules-install", "services-start", "process-start", "process-close", "services-close", "rules-delete"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("%v", events)
	}
	events = nil
	c, s = testConfig(&events)
	h, err = Start(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	s.done <- errors.New("DNS failed")
	if err = h.Wait(); err == nil || !strings.Contains(err.Error(), "DNS failed") {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatal(events)
	}
}
func TestStartupFailsClosed(t *testing.T) {
	for _, stage := range []string{"nonroot", "workspace", "rules", "services", "launch", "missing"} {
		t.Run(stage, func(t *testing.T) {
			var events []string
			c, _ := testConfig(&events)
			switch stage {
			case "nonroot":
				c.Dependencies.EUID = func() int { return 1000 }
			case "workspace":
				c.Dependencies.Workspace = func() error { return errors.New("unsafe workspace") }
			case "rules":
				c.Dependencies.Rules = func(context.Context, string) error { return errors.New("nft denied") }
			case "services":
				c.Dependencies.Services = func(context.Context, *Policy) (Services, error) { return nil, errors.New("bind failed") }
			case "launch":
				c.Dependencies.Launch = func(context.Context, ActorSpec) (Process, error) { return nil, errors.New("exec failed") }
			case "missing":
				c.Dependencies = nil
			}
			h, err := Start(context.Background(), c)
			if err == nil || h != nil {
				t.Fatal("started despite failure")
			}
			if stage != "launch" && strings.Contains(strings.Join(events, ","), "process-start") {
				t.Fatal(events)
			}
			if (stage == "services" || stage == "launch") && events[len(events)-1] != "rules-delete" {
				t.Fatal(events)
			}
		})
	}
}
func TestNFTRules(t *testing.T) {
	rules := nftRules()
	for _, required := range []string{"table inet brood_guest", "meta skuid 1000", "redirect to :5353", "tcp dport 53", "udp dport 53", "ip daddr 127.0.0.1", "ip6 daddr ::1", "3128", "5353", "ct direction reply", "reject with icmpx", "hook output", "hook input"} {
		if !strings.Contains(rules, required) {
			t.Errorf("missing %s", required)
		}
	}
	if strings.Contains(rules, "flush ruleset") {
		t.Fatal("global firewall modification")
	}
	var got string
	err := applyRules(context.Background(), "add table inet brood_guest", func(_ context.Context, name string, args []string, input string) error {
		got = name + " " + strings.Join(args, " ") + " " + input
		return errors.New("denied")
	})
	if err == nil || got != "/usr/sbin/nft -f - add table inet brood_guest" {
		t.Fatalf("%s %v", got, err)
	}
	if _, err := Start(context.Background(), Config{}); err == nil {
		t.Fatal("unset dependencies")
	}
}
