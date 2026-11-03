//go:build linux

// Command brood-guest supervises the dedicated Proxmox coding guest.
// The project directory starts empty; seed bundles are deliberately not installed.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
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
	// stage — стадия supervisor (#155): bootstrap качает артефакты и
	// exec-ается в runtime; runtime сразу ведёт сервисы.
	stage string
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
	// artifacts ставит три бинарных артефакта сервера (#155); reexec
	// подменяет процесс на установленную runtime-стадию.
	artifacts func(context.Context, relay.ClientConfig, []string, func(context.Context, string, io.Reader, int64) error) error
	reexec    func() error
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
		// BROOD_GUEST_STAGE приходит только из env процесса supervisor,
		// не от PVE bootstrap: в allowlist его нет.
		if !ok || !names[key] && key != "BROOD_GUEST_STAGE" {
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
	switch values["BROOD_GUEST_STAGE"] {
	case "":
		c.stage = "bootstrap"
	case "bootstrap", "runtime":
		c.stage = values["BROOD_GUEST_STAGE"]
	default:
		return config{}, errors.New("invalid BROOD_GUEST_STAGE")
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
	// Сеть нужна раньше артефактов (#155): CT получает адрес по DHCP, без
	// него bootstrap не достигнет сервера.
	network, err := d.network(ctx)
	if err != nil {
		return errors.New("guest DHCP initialization failed")
	}
	networkDone := make(chan error, 1)
	go func() { networkDone <- network.Wait(); cancel() }()
	if c.stage != "runtime" {
		// Стадия bootstrap (#155): проверенные артефакты и переход в runtime
		// до секретов и сервисов актора; отказ fail closed.
		if err := bootstrapArtifacts(ctx, c, d); err != nil {
			cancel()
			boundedClose(network)
			return err
		}
		// Успешный exec не возвращает: процесс bootstrap-стадии заменён
		// runtime-бинарём. Сеть закрываем сама: exec не даст вернуться сюда.
		boundedClose(network)
		return nil
	}
	// Артефакты runtime-стадии уже в /opt/brood/bin: bro и claude видны
	// хелперам через PATH, не через копирование в /usr/local/bin.
	if current := os.Getenv("PATH"); current == "" {
		os.Setenv("PATH", "/opt/brood/bin")
	} else {
		os.Setenv("PATH", "/opt/brood/bin:"+current)
	}
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

// artifactRoot — корень артефактов Bootstrap (#155); переназначается тестами.
var artifactRoot = "/opt/brood"

// bootstrapArtifacts ставит проверенные артефакты сервера и переходит
// в runtime-стадию. Причины отказов статичны и ограничены: bootstrap
// не стартует сервисы без проверенных бинарников (fail closed).
func bootstrapArtifacts(ctx context.Context, c config, d dependencies) error {
	if d.artifacts == nil || d.reexec == nil {
		return errors.New("guest bootstrap unavailable")
	}
	if err := d.artifacts(ctx, c.relay, c.hosts, installArtifact); err != nil {
		return errors.New("guest bootstrap failed: artifacts not delivered")
	}
	if err := d.reexec(); err != nil {
		return errors.New("guest bootstrap failed: runtime stage not started")
	}
	return nil
}

// installArtifact ставит один артефакт атомарно: спул в tmp, 0755, fsync
// файла и каталога, rename в bin. Повторная установка идентичного бинарника
// допустима; существующий другой контент или не-файл — отказ.
func installArtifact(_ context.Context, name string, stream io.Reader, size int64) error {
	// Имена закрыты списком манифеста; повторная проверка отсекает
	// произвольные пути из ошибочной доставки.
	if name != "brood-guest" && name != "bro" && name != "claude" {
		return errors.New("guest artifact name rejected")
	}
	dir := filepath.Join(artifactRoot, "bin")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return errors.New("guest artifact directory unavailable")
	}
	if err := os.MkdirAll(filepath.Join(artifactRoot, "tmp"), 0755); err != nil {
		return errors.New("guest artifact staging unavailable")
	}
	destination := filepath.Join(dir, name)
	if existing, err := os.Lstat(destination); err == nil {
		if !existing.Mode().IsRegular() {
			return errors.New("guest artifact destination not a regular file")
		}
		sum, err := fileDigest(destination)
		if err != nil {
			return errors.New("guest artifact destination unreadable")
		}
		received, err := digestOf(stream, size)
		if err != nil {
			return errors.New("guest artifact stream invalid")
		}
		if !bytes.Equal(sum, received) {
			return errors.New("guest artifact differs from the installed binary")
		}
		return nil
	}
	// Спул и rename дают атомарную публикацию; PID гасит гонки процессов.
	spool, err := os.CreateTemp(filepath.Join(artifactRoot, "tmp"), name+"."+strconv.Itoa(os.Getpid())+".")
	if err != nil {
		return errors.New("guest artifact staging unavailable")
	}
	defer os.Remove(spool.Name())
	written, err := io.Copy(spool, io.LimitReader(stream, size))
	if err != nil || written != size {
		spool.Close()
		return errors.New("guest artifact stream invalid")
	}
	if err := spool.Chmod(0755); err != nil {
		spool.Close()
		return errors.New("guest artifact permissions unavailable")
	}
	if err := spool.Sync(); err != nil {
		spool.Close()
		return errors.New("guest artifact sync failed")
	}
	if err := spool.Close(); err != nil {
		return errors.New("guest artifact close failed")
	}
	if err := os.Rename(spool.Name(), destination); err != nil {
		return errors.New("guest artifact publish failed")
	}
	return syncDir(dir)
}

// reexecRuntime подменяет процесс на установленный brood-guest runtime-стадии:
// argv сохраняется, stdio наследуется, из env уходит только стадия.
func reexecRuntime() error {
	env := []string{}
	for _, entry := range os.Environ() {
		// Стадия не наследуется: runtime задаётся ниже, одна и без дублей.
		if strings.HasPrefix(entry, "BROOD_GUEST_STAGE=") {
			continue
		}
		env = append(env, entry)
	}
	return syscall.Exec("/opt/brood/bin/brood-guest", os.Args, append(env, "BROOD_GUEST_STAGE=runtime"))
}

func fileDigest(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	return sum[:], nil
}

func digestOf(stream io.Reader, size int64) ([]byte, error) {
	if size <= 0 || size > 512<<20 {
		return nil, errors.New("invalid artifact size")
	}
	digest := sha256.New()
	written, err := io.Copy(digest, io.LimitReader(stream, size+1))
	if err != nil {
		return nil, err
	}
	if written != size {
		return nil, errors.New("stream shorter than size")
	}
	if extra, _ := io.Copy(io.Discard, stream); extra != 0 {
		return nil, errors.New("stream longer than size")
	}
	return digest.Sum(nil), nil
}

func syncDir(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return errors.New("guest artifact directory unavailable")
	}
	defer handle.Close()
	if err := handle.Sync(); err != nil {
		return errors.New("guest artifact directory sync failed")
	}
	return nil
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
	}, origin: guest.RefreshWorkspaceOrigin, euid: os.Geteuid, network: startDHCP, artifacts: guest.FetchArtifacts, reexec: reexecRuntime, start: func(ctx context.Context, hosts []string) (actorRuntime, error) {
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
