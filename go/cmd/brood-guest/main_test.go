//go:build linux

package main

// FEATURE: Wire the root guest supervisor without exposing bootstrap credentials.
// SPEC: Validate before any effect; dispatch actor before env; actor then relay;
// fail closed and clean children on cancellation or component failure.
// IMPLEMENTATION: Exact actor dispatch, preflight relay validation and ordered
// bounded runtime/relay cleanup. The distribution owns the network.
// RESULT: IMPLEMENTED. Lifecycle regressions covered without host writes.
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
	"math/big"
	"reflect"
	"strings"
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
	return []string{"BROOD_GUEST_RELAY_URL=wss://brood.example", "BROOD_GUEST_ACTOR_UID=actor-1", "BROOD_GUEST_GENERATION=1", "BROOD_GUEST_RELAY_TOKEN=private-token", "BROOD_GUEST_RELAY_HOST_KEY=" + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))}
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
	cases := map[string]string{"BROOD_GUEST_RELAY_URL": "wss://user:private-token@host/path", "BROOD_GUEST_ACTOR_UID": "../private-token", "BROOD_GUEST_GENERATION": "01", "BROOD_GUEST_RELAY_TOKEN": "private-token\n", "BROOD_GUEST_RELAY_HOST_KEY": "private-token", "BROOD_GUEST_CA_PEM_B64": base64.StdEncoding.EncodeToString([]byte("private-token")), "BROOD_GUEST_UNKNOWN": "private-token", "BROOD_GUEST_CA_PEM": "private-token"}
	for key, value := range cases {
		t.Run(key, func(t *testing.T) {
			_, err := parseConfig(replace(env, key, value))
			if err == nil || strings.Contains(err.Error(), "private-token") {
				t.Fatalf("not safely rejected: %v", err)
			}
		})
	}
	for _, key := range []string{"BROOD_GUEST_RELAY_URL", "BROOD_GUEST_ACTOR_UID", "BROOD_GUEST_GENERATION", "BROOD_GUEST_RELAY_TOKEN", "BROOD_GUEST_RELAY_HOST_KEY"} {
		if _, err := parseConfig(replace(env, key, "")); err == nil {
			t.Fatalf("missing %s accepted", key)
		}
	}
	// Переменные прежней egress-политики игнорируются с любым значением.
	ignored, err := parseConfig(append(validEnv(t), "BROOD_GUEST_ALLOWED_HOSTS=[\"127.0.0.1\"]", "BROOD_GUEST_EGRESS_POLICY=not-json"))
	if err != nil || strings.Contains(strings.Join(ignored.bootstrapEnv, "\n"), "EGRESS") || strings.Contains(strings.Join(ignored.bootstrapEnv, "\n"), "ALLOWED_HOSTS") {
		t.Fatalf("legacy egress variables not ignored: %v", err)
	}
	if _, err := parseConfig(append(env, env[0])); err == nil {
		t.Fatal("duplicate accepted")
	}
	if _, err := parseConfig(replace(env, "BROOD_GUEST_CA_PEM_B64", "!!!")); err == nil {
		t.Fatal("invalid base64 accepted")
	}
}

// FEATURE: Opt in to the local step launch lifecycle without enabling inbox transport.
// SPEC: Only an exact opt-in survives bootstrap; neither case advertises an inbox.
// IMPLEMENTATION: Parse the root-only opt-in and require the launch-owning runtime.
func TestStepLaunchBindingConfigDoesNotAdvertiseInbox(t *testing.T) {
	const key = "BROOD_GUEST_STEP_LAUNCH_BINDING"
	env := validEnv(t)
	for _, value := range []string{"", "0", "true", "1 "} {
		if _, err := parseConfig(replace(env, key, value)); err == nil {
			t.Fatalf("accepted invalid step launch opt-in: %q", value)
		}
	}
	for _, enabled := range []bool{false, true} {
		input := env
		if enabled {
			input = replace(env, key, "1")
		}
		c, err := parseConfig(input)
		if err != nil || c.stepLaunchBinding != enabled || c.relay.HandleInbox != nil || c.relay.InboxReceiverID != "" {
			t.Fatalf("unexpected opt-in or relay advertisement: %v", err)
		}
		runtimeConfig, err := parseConfig(runtimeEnv(c))
		if err != nil || runtimeConfig.stepLaunchBinding != enabled || runtimeConfig.stage != "runtime" {
			t.Fatalf("bootstrap lost the launch opt-in: %v", err)
		}
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
	if string(c.relay.CAPEM) != string(ca) || c.relay.Generation != 1 || c.relay.ActorUID != "actor-1" {
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
func (f *fakeService) Quiesce(ctx context.Context, fn func(context.Context) error) error {
	return fn(ctx)
}
func TestStepLaunchBindingRequiresTheOwningRuntime(t *testing.T) {
	runtime := newFake()
	d := dependencies{
		euid:  func() int { return 0 },
		start: func(context.Context) (actorRuntime, error) { return runtime, nil },
		relay: func(context.Context, relay.ClientConfig) error {
			t.Error("relay started without the launch-owning runtime")
			return errors.New("unexpected relay start")
		},
	}
	t.Setenv("PATH", "/usr/bin:/bin")
	env := append(validEnv(t), "BROOD_GUEST_STAGE=runtime", "BROOD_GUEST_STEP_LAUNCH_BINDING=1")
	err := dispatch(context.Background(), nil, func() []string { return env }, d)
	if err == nil || !strings.Contains(err.Error(), "step launch binding") || !runtime.closed {
		t.Fatalf("incomplete opt-in did not fail closed: %v", err)
	}
}

func TestOrchestration(t *testing.T) {
	for _, failure := range []string{"actor", "relay", "service", "signal"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			runtime := newFake()
			var events []string
			d := dependencies{euid: func() int { return 0 }, start: func(context.Context) (actorRuntime, error) {
				events = append(events, "actor")
				if failure == "actor" {
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
				case "service":
					runtime.Close()
				case "signal":
					cancel()
				}
				<-ctx.Done()
				return ctx.Err()
			}}
			t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
			err := dispatch(ctx, nil, func() []string { return append(validEnv(t), "BROOD_GUEST_STAGE=runtime") }, d)
			if failure == "signal" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || strings.Contains(err.Error(), "private-token") {
				t.Fatalf("unsafe error: %v", err)
			}
			want := []string{"actor"}
			if failure != "actor" {
				want = append(want, "relay")
			}
			if !reflect.DeepEqual(events, want) {
				t.Fatalf("events %v want %v", events, want)
			}
			if len(events) == 2 && !runtime.closed {
				t.Fatal("runtime leaked")
			}
		})
	}
}
func TestInvalidConfigBeforeHostPreparation(t *testing.T) {
	err := dispatch(context.Background(), nil, func() []string { return []string{"BROOD_GUEST_UNKNOWN=private-token"} }, dependencies{euid: func() int { return 0 }, prepareHost: func(context.Context) error { t.Fatal("host writes before validation"); return nil }})
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
