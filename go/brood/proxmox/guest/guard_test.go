//go:build linux

package guest

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Failed termination must retain the root policy, including for detached tmux.
func TestDrainDetachedActor(t *testing.T) {
	calls := 0
	if err := drainActorProcesses(context.Background(), func() (bool, error) { calls++; return calls < 3, nil }); err != nil || calls != 3 {
		t.Fatalf("%d %v", calls, err)
	}
	sentinel := errors.New("cannot inspect UID")
	if err := drainActorProcesses(context.Background(), func() (bool, error) { return false, sentinel }); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := drainActorProcesses(ctx, func() (bool, error) { t.Error("swept after cancellation"); return true, nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		status string
		want   bool
		valid  bool
	}{{"Name:\ttmux\nState:\tS (sleeping)\nUid:\t1000\t1000\t1000\t1000\n", true, true}, {"Name:\troot\nState:\tS (sleeping)\nUid:\t0\t0\t0\t0\n", false, true}, {"State:\tZ (zombie)\nUid:\t1000\t1000\t1000\t1000\n", false, true}, {"State:\tS\nUid:\t0\t1000\t0\t0\n", true, true}, {"garbage", false, false}} {
		got, err := actorStatus([]byte(tc.status))
		if got != tc.want || (err == nil) != tc.valid {
			t.Fatalf("%q: %v %v", tc.status, got, err)
		}
	}
}

type closeFailure struct{ Process }

func (p closeFailure) Close() error { return errors.New("descendant still alive") }

type signalProcess struct {
	*fakeProcess
	err error
}

func (p signalProcess) Wait() error { <-p.done; return p.err }
func TestCloseRetainsRulesOnTerminationFailure(t *testing.T) {
	var events []string
	c, _ := testConfig(&events)
	var child *fakeProcess
	c.Dependencies.Launch = func(context.Context, ActorSpec) (Process, error) {
		child = &fakeProcess{done: make(chan struct{}), events: &events}
		return closeFailure{child}, nil
	}
	h, err := Start(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.Close(); err == nil {
		t.Fatal("termination failure hidden")
	}
	if strings.Contains(strings.Join(events, ","), "rules-delete") {
		t.Fatal(events)
	}
	if !strings.Contains(strings.Join(events, ","), "services-close") {
		t.Fatal("failed termination left proxy running")
	}
	_ = child.Close()
}
func TestIntentionalKillIsNotWaitFailure(t *testing.T) {
	var events []string
	c, _ := testConfig(&events)
	cmd := exec.Command("/bin/sh", "-c", "kill -KILL $$")
	err := cmd.Run()
	if err == nil {
		t.Fatal("expected test subprocess signal")
	}
	c.Dependencies.Launch = func(context.Context, ActorSpec) (Process, error) {
		return signalProcess{&fakeProcess{done: make(chan struct{}), events: &events}, err}, nil
	}
	h, err := Start(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.Close(); err != nil {
		t.Fatalf("intentional actor termination: %v", err)
	}
}
func TestMissingDependencyAndCanceledContext(t *testing.T) {
	for _, field := range []string{"EUID", "Lookup", "Workspace", "Rules", "Services", "Launch"} {
		var events []string
		c, _ := testConfig(&events)
		reflect.ValueOf(c.Dependencies).Elem().FieldByName(field).SetZero()
		if h, err := Start(context.Background(), c); h != nil || err == nil || len(events) != 0 {
			t.Fatalf("%s: %v %v", field, events, err)
		}
	}
	var events []string
	c, _ := testConfig(&events)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if h, err := Start(ctx, c); h != nil || !errors.Is(err, context.Canceled) || len(events) != 0 {
		t.Fatalf("%v %v", events, err)
	}
}
func TestWorkspaceUnsafeModesAndFiles(t *testing.T) {
	root := privateTemp(t)
	if err := os.Chmod(root, 0777); err != nil {
		t.Fatal(err)
	}
	if err := prepareWorkspace(root, os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("writable workspace accepted")
	}
	root = privateTemp(t)
	if err := os.WriteFile(root+"/project", nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := prepareWorkspace(root, os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("project file accepted")
	}
}
func TestHTTPRedirectNotFollowed(t *testing.T) {
	p, _ := NewPolicy([]string{"example.com"}, publicLookup)
	dials := 0
	proxy, _ := NewProxy(p, func(context.Context, string, string) (net.Conn, error) {
		dials++
		a, b := net.Pipe()
		go func() {
			defer b.Close()
			buf := make([]byte, 4096)
			_, _ = b.Read(buf)
			_, _ = io.WriteString(b, "HTTP/1.1 302 Found\r\nLocation: http://127.0.0.1/private\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		}()
		return a, nil
	})
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/", nil))
	if rec.Code != 302 || dials != 1 {
		t.Fatalf("%d %d", rec.Code, dials)
	}
}
func TestConnectCloseInterruptsTunnel(t *testing.T) {
	p, _ := NewPolicy([]string{"example.com"}, publicLookup)
	peer := make(chan net.Conn, 1)
	proxy, _ := NewProxy(p, func(context.Context, string, string) (net.Conn, error) { a, b := net.Pipe(); peer <- b; return a, nil })
	server := httptest.NewServer(proxy)
	defer server.Close()
	defer proxy.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	_, _ = io.WriteString(conn, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
	buf := make([]byte, len("HTTP/1.1 200 Connection Established\r\n\r\n"))
	if _, err = io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	b := <-peer
	defer b.Close()
	if err = proxy.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Read(buf); err == nil {
		t.Fatal("tunnel still open")
	}
}
