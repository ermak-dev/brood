//go:build linux

// Command brood-guest supervises the dedicated Proxmox coding guest.
// The project directory starts empty; seed bundles are deliberately not installed.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"brood.dev/go/brood/proxmox/guest"
	"brood.dev/go/brood/proxmox/relay"
)

type config struct {
	hosts []string
	relay relay.ClientConfig
	// origin и originMode не секретны: клон workspace (#147).
	origin, originMode string
}
type service interface {
	Wait() error
	Close() error
}
type actorRuntime interface {
	service
	TerminalDialTarget() string
}
type swarmBroker interface {
	service
	ServeSession(context.Context, relay.SwarmCaller)
}

type dependencies struct {
	app     func() error
	secrets func(context.Context, relay.ClientConfig) error
	actor   func() error
	message func(context.Context) error
	session func(context.Context) error
	mcp     func(context.Context) error
	broker  func(context.Context) (swarmBroker, error)
	model   func(context.Context, relay.ClientConfig) (service, error)
	git     func(context.Context, relay.ClientConfig) (service, error)
	origin  func(context.Context, relay.ClientConfig, []string) (guest.WorkspaceOrigin, error)
	// boot — UID 1000 цель --brood-guest-autostart; autorun — root-цикл,
	// который её запускает до появления сессии Claude.
	boot    func(context.Context) error
	autorun func(context.Context)
	euid    func() int
	network func(context.Context) (service, error)
	start   func(context.Context, []string) (actorRuntime, error)
	relay   func(context.Context, relay.ClientConfig) error
}

func parseConfig(env []string) (config, error) {
	var c config
	names := map[string]bool{"BROOD_GUEST_RELAY_URL": true, "BROOD_GUEST_ACTOR_UID": true, "BROOD_GUEST_GENERATION": true, "BROOD_GUEST_RELAY_TOKEN": true, "BROOD_GUEST_RELAY_HOST_KEY": true, "BROOD_GUEST_CA_PEM_B64": true, "BROOD_GUEST_ALLOWED_HOSTS": true}
	optional := map[string]bool{"BROOD_GUEST_CA_PEM_B64": true}
	values := map[string]string{}
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !strings.HasPrefix(key, "BROOD_GUEST_") {
			continue
		}
		if !ok || !names[key] {
			return c, errors.New("unknown guest configuration variable")
		}
		if _, exists := values[key]; exists {
			return c, errors.New("duplicate guest configuration variable")
		}
		if len(value) > 128*1024 || strings.ContainsAny(value, "\r\n\x00") {
			return c, fmt.Errorf("invalid %s", key)
		}
		values[key] = value
	}
	for key := range names {
		if !optional[key] && values[key] == "" {
			return c, fmt.Errorf("%s is required", key)
		}
	}
	generation, err := strconv.ParseInt(values["BROOD_GUEST_GENERATION"], 10, 64)
	if err != nil || generation <= 0 || strconv.FormatInt(generation, 10) != values["BROOD_GUEST_GENERATION"] {
		return c, errors.New("invalid BROOD_GUEST_GENERATION")
	}
	if err := json.Unmarshal([]byte(values["BROOD_GUEST_ALLOWED_HOSTS"]), &c.hosts); err != nil {
		return c, errors.New("invalid BROOD_GUEST_ALLOWED_HOSTS JSON array")
	}
	if _, err := guest.NewPolicy(c.hosts, func(context.Context, string) ([]net.IP, error) { return nil, nil }); err != nil {
		return c, errors.New("invalid BROOD_GUEST_ALLOWED_HOSTS hostnames")
	}
	ca, err := base64.StdEncoding.Strict().DecodeString(values["BROOD_GUEST_CA_PEM_B64"])
	if err != nil {
		return c, errors.New("invalid BROOD_GUEST_CA_PEM_B64 encoding")
	}
	u, err := url.Parse(values["BROOD_GUEST_RELAY_URL"])
	if err != nil {
		return c, errors.New("invalid BROOD_GUEST_RELAY_URL")
	}
	if port := u.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return c, errors.New("invalid guest relay port")
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return c, errors.New("invalid guest relay port")
	}
	c.relay = relay.ClientConfig{URL: values["BROOD_GUEST_RELAY_URL"], ActorUID: values["BROOD_GUEST_ACTOR_UID"], Generation: generation, Token: values["BROOD_GUEST_RELAY_TOKEN"], HostKey: values["BROOD_GUEST_RELAY_HOST_KEY"], CAPEM: ca, DialTarget: terminalDial, HandleMessage: guest.DeliverMessage, SessionFactory: guest.NewSessionHandler}
	// Отменённый контекст проверяет реальный контракт relay без сетевого запроса.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := relay.RunClient(ctx, c.relay); !errors.Is(err, context.Canceled) {
		return config{}, errors.New("invalid guest relay configuration (URL, identity, token, host key or CA)")
	}
	return c, nil
}
func terminalDial(ctx context.Context) (net.Conn, error) {
	return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", "127.0.0.1:8080")
}
func dispatch(ctx context.Context, args []string, env func() []string, d dependencies) error {
	if len(args) == 1 && args[0] == "--brood-guest-app" {
		return d.app()
	}
	if len(args) == 1 && args[0] == "--brood-guest-actor" {
		return d.actor()
	}
	if len(args) == 1 && args[0] == "--brood-guest-message" {
		return d.message(ctx)
	}
	if len(args) == 1 && args[0] == "--brood-guest-session" {
		return d.session(ctx)
	}
	if len(args) == 1 && args[0] == "--brood-guest-autostart" {
		return d.boot(ctx)
	}
	if len(args) == 1 && args[0] == "mcp" {
		return d.mcp(ctx)
	}
	if len(args) != 0 {
		return errors.New("brood-guest accepts only fixed actor or message helper flags")
	}
	c, err := parseConfig(env())
	if err != nil {
		return err
	}
	if d.euid() == 0 {
		return supervise(ctx, c, d)
	}
	return errors.New("guest supervisor must run as root")
}
func supervise(parent context.Context, c config, d dependencies) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	network, err := d.network(ctx)
	if err != nil {
		return errors.New("guest DHCP initialization failed")
	}
	networkDone := make(chan error, 1)
	go func() { networkDone <- network.Wait(); cancel() }()
	if d.secrets != nil {
		if err := d.secrets(ctx, c.relay); err != nil {
			cancel()
			boundedClose(network)
			return errors.New("guest project secrets initialization failed")
		}
		// Capture only the immutable bootstrap identity, never the relay socket.
		cfg := c.relay
		c.relay.SessionFactory = func(connection context.Context) func(context.Context, relay.SessionRequest) relay.SessionResponse {
			return guest.NewSessionHandlerWithSecrets(connection, func(ctx context.Context) error { return d.secrets(ctx, cfg) })
		}
	}
	if d.origin != nil {
		origin, err := d.origin(ctx, c.relay, c.hosts)
		if err != nil {
			cancel()
			boundedClose(network)
			return errors.New("guest workspace origin initialization failed")
		}
		c.origin, c.originMode = origin.Origin, origin.Mode
	}
	if d.broker != nil {
		broker, err := d.broker(ctx)
		if err != nil {
			cancel()
			boundedClose(network)
			return errors.New("guest swarm broker initialization failed")
		}
		defer broker.Close()
		c.relay.OnSwarmSession = broker.ServeSession
		go func() { broker.Wait(); cancel() }()
	}
	if d.model != nil {
		model, err := d.model(ctx, c.relay)
		if err != nil {
			cancel()
			boundedClose(network)
			return errors.New("guest model gateway initialization failed")
		}
		defer boundedClose(model)
		go func() { model.Wait(); cancel() }()
	}
	// Только gateway-режим открывает 8318: direct-клон идёт через egress proxy.
	if d.git != nil && c.originMode == "gateway" {
		git, err := d.git(ctx, c.relay)
		if err != nil {
			cancel()
			boundedClose(network)
			return errors.New("guest Git gateway initialization failed")
		}
		defer boundedClose(git)
		go func() { git.Wait(); cancel() }()
	}
	runtime, err := d.start(ctx, c.hosts)
	if err != nil {
		cancel()
		boundedClose(network)
		return errors.New("guest policy or actor initialization failed")
	}
	runtimeDone := make(chan error, 1)
	go func() { runtimeDone <- runtime.Wait(); cancel() }()
	relayDone := make(chan error, 1)
	if runtime.TerminalDialTarget() != "127.0.0.1:8080" {
		cancel()
		boundedClose(runtime, network)
		return errors.New("invalid guest terminal target")
	}
	go func() { relayDone <- d.relay(ctx, c.relay); cancel() }()
	// Сессия Claude поднимается без терминала: после секретов, origin и сервисов
	// актора. Отказ автостарта не останавливает supervisor.
	autostartDone := make(chan struct{})
	go func() {
		defer close(autostartDone)
		if d.autorun != nil {
			d.autorun(ctx)
		}
	}()
	<-ctx.Done()
	cancel()
	cleanupErr := boundedClose(runtime, network)
	// Не отдаём произвольные ошибки библиотек: bootstrap может попасть в их текст.
	select {
	case <-relayDone:
	case <-time.After(5 * time.Second):
		return errors.New("guest relay shutdown timed out")
	}
	select {
	case <-autostartDone:
	case <-time.After(5 * time.Second):
		return errors.New("guest session autostart shutdown timed out")
	}
	if cleanupErr != nil {
		return cleanupErr
	}
	if parent.Err() != nil {
		return nil
	}
	return errors.New("guest DHCP, actor service or relay stopped unexpectedly")
}
func boundedClose(services ...service) error {
	done := make(chan error, 1)
	go func() {
		var result error
		for _, s := range services {
			if err := s.Close(); !cancellationOnly(err) {
				result = errors.New("guest cleanup failed")
			}
		}
		done <- result
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		return errors.New("guest cleanup timed out")
	}
}
func cancellationOnly(err error) bool {
	if err == nil || err == context.Canceled {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if !cancellationOnly(child) {
				return false
			}
		}
		return true
	}
	if wrapped := errors.Unwrap(err); wrapped != nil {
		return cancellationOnly(wrapped)
	}
	return false
}
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	d := dependencies{app: func() error { return guest.RunProjectApp(os.Stderr) }, boot: guest.RunSessionAutostart, autorun: func(ctx context.Context) {
		guest.AutostartSession(ctx, func(err error) { fmt.Fprintln(os.Stderr, err) })
	}, secrets: guest.RefreshProjectSecrets, actor: guest.RunActor, message: runMessageHelper, session: runSessionHelper, mcp: runMCP, broker: func(ctx context.Context) (swarmBroker, error) { return guest.StartSwarmBroker(ctx) }, model: func(ctx context.Context, cfg relay.ClientConfig) (service, error) {
		return guest.StartModelGateway(ctx, cfg)
	}, git: func(ctx context.Context, cfg relay.ClientConfig) (service, error) {
		return guest.StartGitGateway(ctx, cfg)
	}, origin: guest.RefreshWorkspaceOrigin, euid: os.Geteuid, network: startDHCP, start: func(ctx context.Context, hosts []string) (actorRuntime, error) {
		return guest.Start(ctx, guest.Config{Hosts: hosts, Dependencies: guest.SystemDependencies()})
	}, relay: relay.RunClient}
	err := dispatch(ctx, os.Args[1:], func() []string {
		env := os.Environ()
		os.Clearenv()
		for _, entry := range helperEnv() {
			key, value, _ := strings.Cut(entry, "=")
			_ = os.Setenv(key, value)
		}
		return env
	}, d)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
