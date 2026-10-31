//go:build linux

package main

// FEATURE: Wire the root guest supervisor without exposing bootstrap credentials.
// SPEC: Validate before DHCP; dispatch actor before env; network then policy then
// relay; fail closed and clean children on cancellation or component failure.
// IMPLEMENTATION: Exact actor dispatch, preflight relay/policy validation, isolated
// DHCP renewal supervision and ordered bounded runtime/relay cleanup.
// RESULT: IMPLEMENTED. Lifecycle regressions covered without network writes.
// STATUS: DONE

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/relay"
	"golang.org/x/crypto/ssh"
)

func validEnv(t *testing.T) []string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return []string{"BROOD_GUEST_RELAY_URL=wss://brood.example", "BROOD_GUEST_ACTOR_UID=actor-1", "BROOD_GUEST_GENERATION=1", "BROOD_GUEST_RELAY_TOKEN=private-token", "BROOD_GUEST_RELAY_HOST_KEY=" + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))), "BROOD_GUEST_ALLOWED_HOSTS=[\"github.com\"]"}
}
func replace(env []string, key, value string) []string {
	result := append([]string{}, env...)
	for i, s := range result {
		if strings.HasPrefix(s, key+"=") {
			result[i] = key + "=" + value
			return result
		}
	}
	return append(result, key+"="+value)
}
func TestConfigValidation(t *testing.T) {
	env := validEnv(t)
	if _, err := parseConfig(env); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{"BROOD_GUEST_RELAY_URL": "wss://user:private-token@host/path", "BROOD_GUEST_ACTOR_UID": "../private-token", "BROOD_GUEST_GENERATION": "01", "BROOD_GUEST_RELAY_TOKEN": "private-token\n", "BROOD_GUEST_RELAY_HOST_KEY": "private-token", "BROOD_GUEST_CA_PEM_B64": base64.StdEncoding.EncodeToString([]byte("private-token")), "BROOD_GUEST_ALLOWED_HOSTS": "[\"127.0.0.1\"]", "BROOD_GUEST_UNKNOWN": "private-token", "BROOD_GUEST_CA_PEM": "private-token"}
	for key, value := range cases {
		t.Run(key, func(t *testing.T) {
			_, err := parseConfig(replace(env, key, value))
			if err == nil || strings.Contains(err.Error(), "private-token") {
				t.Fatalf("not safely rejected: %v", err)
			}
		})
	}
	for _, key := range []string{"BROOD_GUEST_RELAY_URL", "BROOD_GUEST_ACTOR_UID", "BROOD_GUEST_GENERATION", "BROOD_GUEST_RELAY_TOKEN", "BROOD_GUEST_RELAY_HOST_KEY", "BROOD_GUEST_ALLOWED_HOSTS"} {
		if _, err := parseConfig(replace(env, key, "")); err == nil {
			t.Fatalf("missing %s accepted", key)
		}
	}
	if _, err := parseConfig(append(env, env[0])); err == nil {
		t.Fatal("duplicate accepted")
	}
	if _, err := parseConfig(replace(env, "BROOD_GUEST_CA_PEM_B64", "!!!")); err == nil {
		t.Fatal("invalid base64 accepted")
	}
}
func TestConfigDecodedCAAndExactEndpoint(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, IsCA: true, BasicConstraintsValid: true, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	env := replace(validEnv(t), "BROOD_GUEST_CA_PEM_B64", base64.StdEncoding.EncodeToString(ca))
	env = replace(env, "BROOD_GUEST_RELAY_URL", "wss://brood.example:443/api/proxmox/guest/actor-1/1")
	c, err := parseConfig(env)
	if err != nil {
		t.Fatal(err)
	}
	if string(c.relay.CAPEM) != string(ca) || c.relay.Generation != 1 || c.relay.ActorUID != "actor-1" || !reflect.DeepEqual(c.hosts, []string{"github.com"}) {
		t.Fatal("configuration changed")
	}
}
func TestRelayURLPortValidation(t *testing.T) {
	for _, url := range []string{"wss://brood.example:99999", "wss://brood.example:0", "wss://brood.example:"} {
		if _, err := parseConfig(replace(validEnv(t), "BROOD_GUEST_RELAY_URL", url)); err == nil {
			t.Fatalf("invalid port accepted: %s", url)
		}
	}
}
func TestDispatch(t *testing.T) {
	calls := 0
	d := dependencies{actor: func() error { calls++; return nil }}
	env := func() []string { t.Fatal("actor read root env"); return nil }
	if err := dispatch(context.Background(), []string{"--brood-guest-actor"}, env, d); err != nil || calls != 1 {
		t.Fatal(err, calls)
	}
	for _, args := range [][]string{{"sh"}, {"--brood-guest-actor", "extra"}, {"--exec", "id"}} {
		if err := dispatch(context.Background(), args, env, d); err == nil {
			t.Fatal("args accepted", args)
		}
	}
}

type fakeService struct {
	done   chan struct{}
	closed bool
}

func newFake() *fakeService        { return &fakeService{done: make(chan struct{})} }
func (f *fakeService) Wait() error { <-f.done; return errors.New("private-token") }
func (f *fakeService) Close() error {
	if !f.closed {
		f.closed = true
		close(f.done)
	}
	return nil
}
func (f *fakeService) TerminalDialTarget() string { return "127.0.0.1:8080" }
func TestOrchestration(t *testing.T) {
	for _, failure := range []string{"network", "policy", "relay", "dhcp", "service", "signal"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			network, runtime := newFake(), newFake()
			var events []string
			d := dependencies{euid: func() int { return 0 }, network: func(context.Context) (service, error) {
				events = append(events, "network")
				if failure == "network" {
					return nil, errors.New("private-token")
				}
				return network, nil
			}, start: func(context.Context, []string) (actorRuntime, error) {
				events = append(events, "policy")
				if failure == "policy" {
					return nil, errors.New("private-token")
				}
				return runtime, nil
			}, relay: func(ctx context.Context, c relay.ClientConfig) error {
				events = append(events, "relay")
				if c.DialTarget == nil {
					t.Error("no terminal target")
				}
				switch failure {
				case "relay":
					return errors.New("private-token")
				case "dhcp":
					network.Close()
				case "service":
					runtime.Close()
				case "signal":
					cancel()
				}
				<-ctx.Done()
				return ctx.Err()
			}}
			err := dispatch(ctx, nil, func() []string { return validEnv(t) }, d)
			if failure == "signal" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || strings.Contains(err.Error(), "private-token") {
				t.Fatalf("unsafe error: %v", err)
			}
			want := []string{"network"}
			if failure != "network" {
				want = append(want, "policy")
			}
			if failure != "network" && failure != "policy" {
				want = append(want, "relay")
			}
			if !reflect.DeepEqual(events, want) {
				t.Fatalf("events %v want %v", events, want)
			}
			if failure != "network" && !network.closed {
				t.Fatal("DHCP leaked")
			}
			if len(events) == 3 && !runtime.closed {
				t.Fatal("runtime leaked")
			}
		})
	}
}
func TestInvalidConfigBeforeNetwork(t *testing.T) {
	err := dispatch(context.Background(), nil, func() []string { return []string{"BROOD_GUEST_UNKNOWN=private-token"} }, dependencies{euid: func() int { return 0 }, network: func(context.Context) (service, error) { t.Fatal("network writes before validation"); return nil, nil }})
	if err == nil || strings.Contains(err.Error(), "private-token") {
		t.Fatal(err)
	}
}

type failedClose struct{ *fakeService }

func (f failedClose) Close() error { f.fakeService.Close(); return errors.New("private-token") }
func TestCleanupFailureRedacted(t *testing.T) {
	if err := boundedClose(failedClose{newFake()}); err == nil || strings.Contains(err.Error(), "private-token") {
		t.Fatalf("lost or unsafe cleanup error: %v", err)
	}
}
func TestDHCPStopsHelpersOnInitializationExit(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	pidfile := filepath.Join(dir, "pid")
	var cmd *exec.Cmd
	command := func(ctx context.Context, hook string) *exec.Cmd {
		cmd = dhcpCommand(ctx, hook)
		cmd.Path = "/bin/sh"
		cmd.Args = []string{"/bin/sh", "-c", `sleep 10 & printf '%s' "$!" > "$1"; exit 1`, "test", pidfile}
		return cmd
	}
	_, err := runDHCP(context.Background(), state, command, time.Second)
	if cmd != nil && cmd.Process != nil {
		defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if err == nil {
		t.Fatal("expected initialization failure")
	}
	raw, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(300 * time.Millisecond)
	for {
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if os.IsNotExist(err) || strings.Contains(string(stat), ") Z ") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("root DHCP descendant survived initialization failure")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func TestDHCPSupervision(t *testing.T) {
	for _, mode := range []string{"renew", "hook-failure", "exit", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			dir := filepath.Join(t.TempDir(), "state")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			command := func(ctx context.Context, hook string) *exec.Cmd {
				cmd := dhcpCommand(ctx, hook)
				cmd.Path = "/bin/sh"
				script := `state=${0%/*}; : > "$state/ready"; exec sleep 10`
				switch mode {
				case "hook-failure":
					script = `state=${0%/*}; : > "$state/ready"; sleep .1; : > "$state/failed"; exec sleep 10`
				case "exit":
					script = "exit 1"
				case "timeout":
					script = "exec sleep 10"
				}
				cmd.Args = []string{"/bin/sh", "-c", script, hook}
				return cmd
			}
			s, err := runDHCP(ctx, dir, command, 150*time.Millisecond)
			if mode == "exit" || mode == "timeout" {
				if err == nil {
					s.Close()
					t.Fatal("DHCP failure accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if mode == "renew" {
				select {
				case <-time.After(300 * time.Millisecond):
				case <-waitService(s):
					t.Fatal("acquisition deadline killed renewal client")
				}
			} else {
				select {
				case <-waitService(s):
				case <-time.After(time.Second):
					t.Fatal("hook failure not fatal")
				}
			}
		})
	}
}
func waitService(s service) <-chan struct{} {
	done := make(chan struct{})
	go func() { s.Wait(); close(done) }()
	return done
}
func TestDHCPCommand(t *testing.T) {
	cmd := dhcpCommand(context.Background(), "/run/private/hook")
	if cmd.Path != "/bin/busybox" || !reflect.DeepEqual(cmd.Args, []string{"/bin/busybox", "udhcpc", "-f", "-n", "-i", "eth0", "-s", "/run/private/hook", "-t", "5", "-T", "3"}) {
		t.Fatal(cmd.Path, cmd.Args)
	}
	if !reflect.DeepEqual(cmd.Env, []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root", "LANG=C"}) {
		t.Fatal("unclean env", cmd.Env)
	}
	if cmd.WaitDelay <= 0 || cmd.WaitDelay > 5*time.Second {
		t.Fatal("unbounded wait")
	}
	if !strings.Contains(dhcpHook, "/usr/local/libexec/proxmox-guest-dhcp.sh") || !strings.Contains(dhcpHook, "failed") {
		t.Fatal("no trusted fatal hook")
	}
}
