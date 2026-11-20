//go:build linux

// Command brood-guest is the brood agent inside a worker container: a stock
// Debian 12 system container where systemd runs it as root in
// brood-agent.service with BROOD_GUEST_* read from /etc/brood/agent.env.
// The distribution configures the network and the actor uses it without
// restriction; the agent prepares the actor account, directories and Debian
// packages itself at start. The project directory starts empty.
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"brood.dev/go/brood/proxmox/guest"
	"brood.dev/go/brood/proxmox/guest/deliverylog"
	"brood.dev/go/brood/proxmox/relay"
)

type config struct {
	relay             relay.ClientConfig
	deliveryDirectory string
	stepLaunchBinding bool
	supervisorInbox   bool
	// workspace не секретен: origin и режим клона (#147) и Git-идентичность
	// актора (#249), как их отдал сервер.
	workspace guest.WorkspaceOrigin
	// stage — стадия supervisor (#155): bootstrap качает артефакты и
	// exec-ается в runtime; runtime сразу ведёт сервисы.
	stage string
	// bootstrapEnv — принятые BROOD_GUEST_* bootstrap env без стадии: main
	// очищает окружение процесса сразу после разбора, поэтому runtime-стадия
	// получает их отсюда, а не из os.Environ.
	bootstrapEnv []string
}
type service interface {
	Wait() error
	Close() error
}
type actorRuntime interface {
	service
	TerminalDialTarget() string
	// Quiesce runs its callback while no actor process exists (#183).
	Quiesce(context.Context, func(context.Context) error) error
}
type swarmBroker interface {
	service
	ServeSession(context.Context, relay.SwarmCaller)
}

// preparedInboxStart activates the exact inbox only after the root runtime exists.
type preparedInboxStart func(context.Context, *guest.Runtime, *relay.ClientConfig) (io.Closer, error)

type dependencies struct {
	deliveryDirectory func(context.Context) (string, error)
	deliveryJournal   func(context.Context, string, deliverylog.Scope) (*deliverylog.Journal, error)
	prepareInbox      func(context.Context, string) (preparedInboxStart, error)
	app               func() error
	inboxApp          func() error
	secrets           func(context.Context, relay.ClientConfig) error
	prepareSecrets    func(context.Context, relay.ClientConfig) (func() error, error)
	actor             func() error
	heldActor         func() error
	attach            func(context.Context) error
	message           func(context.Context) error
	inbox             func(context.Context) error
	session           func(context.Context) error
	mcp               func(context.Context) error
	broker            func(context.Context) (swarmBroker, error)
	model             func(context.Context, relay.ClientConfig) (service, error)
	git               func(context.Context, relay.ClientConfig) (service, error)
	origin            func(context.Context, relay.ClientConfig) (guest.WorkspaceOrigin, error)
	// artifacts читает манифест сервера и отдаёт его установщику набора
	// (#155, #184); reexec подменяет процесс на runtime-стадию набора.
	artifacts func(context.Context, relay.ClientConfig, guest.ArtifactRelease) error
	reexec    func(executable string, env []string) error
	// boot — UID 1000 цель --brood-guest-autostart; autorun — root-цикл,
	// который её запускает до появления сессии Claude.
	boot    func(context.Context) error
	autorun func(context.Context)
	euid    func() int
	// prepareHost доводит стоковый Debian до того, что ждёт runtime (учётная
	// запись актора, каталоги, пакеты), до секретов и до актора; nil в тестах.
	prepareHost func(context.Context) error
	start       func(context.Context) (actorRuntime, error)
	relay       func(context.Context, relay.ClientConfig) error
}

// helperEnv — служебное окружение root-процесса агента: main заменяет им то,
// что передал systemd, чтобы BROOD_GUEST_* не утекли потомкам.
func helperEnv() []string {
	return []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root", "LANG=C"}
}

// ignoredEnv — переменные прежней egress-политики. Сеть актора больше не
// ограничена: старый сервер ещё может их прислать, агент их не читает.
var ignoredEnv = map[string]bool{"BROOD_GUEST_ALLOWED_HOSTS": true, "BROOD_GUEST_EGRESS_POLICY": true}

func parseConfig(env []string) (config, error) {
	var c config
	const (
		stepLaunchBindingEnv = "BROOD_GUEST_STEP_LAUNCH_BINDING"
		supervisorInboxEnv   = "BROOD_GUEST_SUPERVISOR_INBOX"
	)
	names := map[string]bool{"BROOD_GUEST_RELAY_URL": true, "BROOD_GUEST_ACTOR_UID": true, "BROOD_GUEST_GENERATION": true, "BROOD_GUEST_RELAY_TOKEN": true, "BROOD_GUEST_RELAY_HOST_KEY": true, "BROOD_GUEST_CA_PEM_B64": true, "BROOD_GUEST_DELIVERY_STATE_DIR": true, stepLaunchBindingEnv: true, supervisorInboxEnv: true}
	optional := map[string]bool{"BROOD_GUEST_CA_PEM_B64": true, "BROOD_GUEST_DELIVERY_STATE_DIR": true, stepLaunchBindingEnv: true, supervisorInboxEnv: true}
	values := map[string]string{}
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !strings.HasPrefix(key, "BROOD_GUEST_") {
			continue
		}
		if ok && ignoredEnv[key] {
			continue
		}
		// BROOD_GUEST_STAGE приходит только из env процесса supervisor,
		// не из agent.env: в allowlist его нет.
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
	if directory, exists := values["BROOD_GUEST_DELIVERY_STATE_DIR"]; exists {
		if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
			return config{}, errors.New("invalid BROOD_GUEST_DELIVERY_STATE_DIR")
		}
		c.deliveryDirectory = directory
	}
	if value, present := values[stepLaunchBindingEnv]; present {
		if value != "1" {
			return config{}, fmt.Errorf("invalid %s", stepLaunchBindingEnv)
		}
		c.stepLaunchBinding = true
	}
	if value, present := values[supervisorInboxEnv]; present {
		if value != "1" {
			return config{}, fmt.Errorf("invalid %s", supervisorInboxEnv)
		}
		c.supervisorInbox = true
		c.stepLaunchBinding = true
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
	keys := make([]string, 0, len(values))
	for key := range values {
		if key != "BROOD_GUEST_STAGE" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		c.bootstrapEnv = append(c.bootstrapEnv, key+"="+values[key])
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
	if len(args) == 1 && args[0] == "--brood-guest-inbox-app" && d.inboxApp != nil {
		return d.inboxApp()
	}
	if len(args) == 1 && args[0] == "--brood-guest-actor" {
		return d.actor()
	}
	if len(args) == 1 && args[0] == "--brood-guest-actor-held" && d.heldActor != nil {
		return d.heldActor()
	}
	if len(args) == 1 && args[0] == "--brood-guest-attach" {
		return d.attach(ctx)
	}
	if len(args) == 1 && args[0] == "--brood-guest-message" {
		return d.message(ctx)
	}
	if len(args) == 1 && args[0] == "--brood-guest-inbox" {
		return d.inbox(ctx)
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
	// Сеть настраивает дистрибутив: агент её не трогает. Пока она не
	// поднялась, bootstrap отказывает, и systemd перезапускает сервис.
	// Подготовку ведут обе стадии: bootstrap без неё может не иметь корневых
	// сертификатов для сервера, а требования runtime принадлежат скачанному
	// набору, а не бинарю установщика. Когда всё на месте, это несколько stat.
	if d.prepareHost != nil {
		if err := d.prepareHost(ctx); err != nil {
			return errors.New("guest host preparation failed: " + err.Error())
		}
	}
	if c.stage != "runtime" {
		// Стадия bootstrap (#155): проверенные артефакты и переход в runtime
		// до секретов и сервисов актора; отказ fail closed.
		// Успешный exec не возвращает.
		return bootstrapArtifacts(ctx, c, d)
	}
	var err error
	// Артефакты runtime-стадии уже в /opt/brood/bin (ссылка на проверенный
	// набор, #184): bro и claude видны хелперам через PATH, не через
	// копирование в /usr/local/bin.
	if current := os.Getenv("PATH"); current == "" {
		os.Setenv("PATH", "/opt/brood/bin")
	} else {
		os.Setenv("PATH", "/opt/brood/bin:"+current)
	}
	if c.deliveryDirectory == "" && d.deliveryDirectory != nil {
		c.deliveryDirectory, err = d.deliveryDirectory(ctx)
		if err != nil {
			cancel()
			return errors.New("guest delivery image configuration unavailable or unsafe")
		}
	}
	var startInbox preparedInboxStart
	if c.supervisorInbox {
		if c.deliveryDirectory == "" {
			cancel()
			return errors.New("guest supervisor inbox requires durable delivery state")
		}
		if d.prepareInbox != nil {
			startInbox, err = d.prepareInbox(ctx, c.deliveryDirectory)
		}
		if err != nil || startInbox == nil {
			cancel()
			return errors.New("guest supervisor inbox requires a prepared managed loader")
		}
	}
	if c.deliveryDirectory != "" {
		if d.deliveryJournal == nil {
			cancel()
			return errors.New("guest delivery journal initialization unavailable")
		}
		journal, err := d.deliveryJournal(ctx, c.deliveryDirectory, deliverylog.Scope{ActorUID: c.relay.ActorUID, Incarnation: c.relay.Generation})
		if err != nil || journal == nil {
			if journal != nil {
				journal.Close()
			}
			cancel()
			return errors.New("guest delivery journal initialization failed")
		}
		defer journal.Close()
		if journal.Identifier() == "" {
			cancel()
			return errors.New("guest delivery journal identity unavailable")
		}
		c.relay.HandleMessage = guest.NewDurableMessageHandler(journal)
		c.relay.MessageJournalID = journal.Identifier()
		c.relay.MessageDeduplication = true
	}
	// Capture only the immutable bootstrap identity, never the relay socket.
	cfg := c.relay
	var refresh func(context.Context) error
	if d.secrets != nil {
		if err := d.secrets(ctx, c.relay); err != nil {
			cancel()
			return errors.New("guest project secrets initialization failed")
		}
		refresh = func(ctx context.Context) error { return d.secrets(ctx, cfg) }
	}
	if d.origin != nil {
		origin, err := d.origin(ctx, c.relay)
		if err != nil {
			cancel()
			return errors.New("guest workspace origin initialization failed")
		}
		c.workspace = origin
	}
	if d.broker != nil {
		broker, err := d.broker(ctx)
		if err != nil {
			cancel()
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
			return errors.New("guest model gateway initialization failed")
		}
		defer boundedClose(model)
		go func() { model.Wait(); cancel() }()
	}
	// Только gateway-режим открывает 8318: direct-клон ходит к origin сам.
	if d.git != nil && c.workspace.Mode == "gateway" {
		git, err := d.git(ctx, c.relay)
		if err != nil {
			cancel()
			return errors.New("guest Git gateway initialization failed")
		}
		defer boundedClose(git)
		go func() { git.Wait(); cancel() }()
	}
	runtime, err := d.start(ctx)
	if err != nil {
		cancel()
		return errors.New("guest actor initialization failed")
	}
	// Подпись — от bootstrap-идентичности root, остановка — через сам runtime.
	var attestor *guest.WorkspaceAttestor
	owner, ok := runtime.(*guest.Runtime)
	if c.stepLaunchBinding || startInbox != nil {
		if !ok || owner == nil {
			cancel()
			err := errors.New("guest step launch binding requires the owning runtime")
			if runtime == nil || ok {
				return err
			}
			return errors.Join(err, boundedClose(runtime))
		}
		if startInbox != nil {
			broker, err := startInbox(ctx, owner, &c.relay)
			if broker != nil {
				defer broker.Close()
			}
			if err != nil || broker == nil || c.relay.HandleInbox == nil || c.relay.InboxReceiverID == "" {
				cancel()
				return errors.Join(errors.New("guest supervisor inbox broker initialization failed"), boundedClose(runtime))
			}
		}
		attestor = owner.NewStepLaunchWorkspaceAttestor(c.relay, c.workspace)
	} else if ok && owner != nil {
		attestor = owner.NewWorkspaceAttestor(cfg, c.workspace)
	} else {
		attestor = guest.NewWorkspaceAttestor(cfg, c.workspace, runtime.Quiesce)
	}
	if d.prepareSecrets != nil {
		attestor.PrepareSecrets = func(ctx context.Context) (func() error, error) {
			return d.prepareSecrets(ctx, cfg)
		}
	}
	c.relay.SessionFactory = func(connection context.Context) func(context.Context, relay.SessionRequest) relay.SessionResponse {
		return releaseSessions(artifactRoot, guest.NewSupervisorSessionHandler(connection, refresh, attestor))
	}
	runtimeDone := make(chan error, 1)
	go func() { runtimeDone <- runtime.Wait(); cancel() }()
	relayDone := make(chan error, 1)
	if runtime.TerminalDialTarget() != "127.0.0.1:8080" {
		cancel()
		boundedClose(runtime)
		return errors.New("invalid guest terminal target")
	}
	go func() { relayDone <- d.relay(ctx, c.relay); cancel() }()
	// Persistent поднимает сессию после сервисов; ephemeral ждёт workspace_attest,
	// который готовит точный source до первой модели. Отказ автостарта не завершает supervisor.
	autostartDone := make(chan struct{})
	go func() {
		defer close(autostartDone)
		if c.workspace.Ephemeral == nil && d.autorun != nil {
			d.autorun(ctx)
		}
	}()
	<-ctx.Done()
	cancel()
	cleanupErr := boundedClose(runtime)
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
	return errors.New("guest actor service or relay stopped unexpectedly")
}

// artifactRoot: корень наборов артефактов Bootstrap (#155, #184);
// переназначается тестами.
var artifactRoot = "/opt/brood"

// bootstrapArtifacts ставит проверенный набор артефактов сервера, переключает
// на него /opt/brood/bin (#184) и переходит в runtime-стадию этого набора.
// Причины отказов статичны и ограничены: bootstrap не стартует сервисы без
// проверенного серверного манифеста и набора (fail closed).
func bootstrapArtifacts(ctx context.Context, c config, d dependencies) error {
	if d.artifacts == nil || d.reexec == nil {
		return errors.New("guest bootstrap unavailable")
	}
	var executable string
	var releaseErr error
	release := func(ctx context.Context, manifest guest.ArtifactManifest, download guest.ArtifactDownload) error {
		executable, releaseErr = installRelease(ctx, artifactRoot, manifest, download, warnBootstrap)
		return releaseErr
	}
	if err := d.artifacts(ctx, c.relay, release); err != nil || executable == "" {
		if releaseErr != nil {
			return errors.New("guest bootstrap failed: " + releaseErr.Error())
		}
		return errors.New("guest bootstrap failed: artifacts not delivered")
	}
	if err := d.reexec(executable, runtimeEnv(c)); err != nil {
		return errors.New("guest bootstrap failed: runtime stage not started")
	}
	return nil
}

// warnBootstrap печатает в журнал сервиса то, что не остановило старт: текст
// статичный, как у отказов bootstrap.
func warnBootstrap(err error) {
	fmt.Fprintln(os.Stderr, "guest bootstrap warning: "+err.Error())
}

// runtimeEnv — окружение runtime-стадии: служебные PATH/HOME/LANG, исходные
// BROOD_GUEST_* bootstrap env и ровно одна стадия runtime.
func runtimeEnv(c config) []string {
	env := append(helperEnv(), c.bootstrapEnv...)
	return append(env, "BROOD_GUEST_STAGE=runtime")
}

// reexecRuntime подменяет процесс на brood-guest проверенного набора
// (releases/<id>/brood-guest, на него же указывает /opt/brood/bin): argv
// сохраняется, stdio наследуется, окружение собрано runtimeEnv.
func reexecRuntime(executable string, env []string) error {
	return syscall.Exec(executable, os.Args, env)
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
	d := dependencies{deliveryDirectory: func(ctx context.Context) (string, error) {
		return guest.ReadSupervisorDeliveryDirectory(ctx, "/etc/brood")
	}, deliveryJournal: guest.OpenSupervisorDeliveryJournal, prepareInbox: func(ctx context.Context, directory string) (preparedInboxStart, error) {
		profile, err := guest.PrepareStepInbox(ctx, directory)
		if err != nil {
			return nil, err
		}
		return profile.Start, nil
	}, app: func() error { return guest.RunProjectApp(os.Stderr) }, inboxApp: func() error { return guest.RunInboxProjectApp(os.Stderr) }, boot: guest.RunSessionAutostart, autorun: func(ctx context.Context) {
		guest.AutostartSession(ctx, func(err error) { fmt.Fprintln(os.Stderr, err) })
	}, secrets: guest.RefreshProjectSecrets, prepareSecrets: guest.PrepareProjectSecrets,
		actor: guest.RunActor, heldActor: guest.RunHeldActor, attach: guest.RunPTYAttach, message: runMessageHelper,
		inbox: func(ctx context.Context) error { return guest.RunInboxHelper(ctx, os.Stdin, os.Stdout) }, session: runSessionHelper, mcp: runMCP, broker: func(ctx context.Context) (swarmBroker, error) { return guest.StartSwarmBroker(ctx) }, model: func(ctx context.Context, cfg relay.ClientConfig) (service, error) {
			return guest.StartModelGateway(ctx, cfg)
		}, git: func(ctx context.Context, cfg relay.ClientConfig) (service, error) {
			return guest.StartGitGateway(ctx, cfg)
		}, origin: refreshWorkspaceBootstrap, euid: os.Geteuid, artifacts: guest.FetchArtifacts, reexec: reexecRuntime, prepareHost: func(ctx context.Context) error {
			notices, err := guest.PrepareHost(ctx)
			for _, notice := range notices {
				fmt.Fprintln(os.Stderr, "guest host notice: "+notice)
			}
			return err
		}, start: func(ctx context.Context) (actorRuntime, error) {
			return guest.Start(ctx, guest.Config{Dependencies: guest.SystemDependencies()})
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
		if !errors.Is(err, guest.ErrInboxHandoffWaiting) {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
}
