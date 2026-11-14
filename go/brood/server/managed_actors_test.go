package server

// FEATURE: Durable managed actor dispatch and honest runtime readiness.
// SPEC: Persist exact UID/CTID/owner intent before API writes, retain reservations
// on uncertainty, require task+relay+HTTP readiness, and preserve native ACL/UID binding.
// IMPLEMENTATION: Stage-fenced tasks, positive global cleanup proof, fresh reset
// ownership, cancellation-safe evidence, and the exact immutable guest bootstrap.
// RESULT: IMPLEMENTED — task evidence and capacity survive every uncertain stage.
// STATUS: DONE

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

type managedFakePVE struct {
	mu                              sync.Mutex
	server                          *Server
	nextIDs                         []int
	spec                            proxmox.CTSpec
	exists, running                 bool
	creates, starts, stops, deletes int
	bootstraps                      int
	uncertain                       bool
	uncertainStart                  bool
	uncertainDelete                 bool
	// deleteReadFailures — сколько раз Delete откажет на чтении владения, до
	// записи (#277).
	deleteReadFailures int
	// beforeDelete вызывается в начале Delete, после наблюдения прохода и
	// до чтений Delete (#277).
	beforeDelete func()
	// afterDelete вызывается, когда PVE принял Delete, до ответа (#277).
	afterDelete                    func()
	uncertainWithUPID              bool
	bootstrapError                 error
	inspectError, absenceError     error
	taskErrors                     map[string]error
	startUPID                      string
	waited                         []string
	absenceChecks                  int
	inspectStarted, inspectRelease chan struct{}
	deletedWhileRunning            bool
	// envs — bootstrap env каждого Create и ConfigureBootstrap по порядку.
	envs []proxmox.BootstrapEnv
	// uncertainStop — Stop без UPID с неизвестным исходом (#133).
	uncertainStop bool
	// bootstrapReadFailures и startReadFailures — сколько раз
	// ConfigureBootstrap и Start откажут на чтении PVE (владение, digest,
	// статус), до любой записи (#133).
	bootstrapReadFailures, startReadFailures int
	// stopWait — сколько длится ожидание задачи stop-task (#133).
	stopWait time.Duration
	// bootstrapBudgets — остаток срока прохода у каждого ConfigureBootstrap.
	bootstrapBudgets []time.Duration
	// afterStop и afterStart вызываются, когда PVE принял задачу, до ответа.
	afterStop, afterStart func()
	// beforeStart вызывается в начале Start, до чтений PVE; отменённый к
	// тому времени контекст отказывает чтению, как у клиента (#133).
	beforeStart func()
	// startError — явный отказ Start до задачи (403, ErrOwnership): CT не
	// стартует (#133).
	startError error
	// digest — версия конфигурации CT: растёт с каждой её записью (Create,
	// bootstrap, правка мимо brood в тесте), InspectOwned отдаёт её
	// digest-ом (#133).
	digest int
	// bootstrapUncertain — сколько следующих записей bootstrap PVE
	// применяет, но отвечает неоднозначно (потерянный ответ, 5xx) (#133).
	bootstrapUncertain int
	// duringBootstrap вызывается после применённой записи bootstrap, до
	// ответа; отменённый к тому времени контекст делает ответ
	// неоднозначным, как у клиента (#133).
	duringBootstrap func()
	// stopReadFailures — сколько раз Stop откажет на чтении владения, до
	// записи (#275).
	stopReadFailures int
	// stopRefusesStopped — Stop остановленной CT отвечает, как
	// proxmox.Client на HTTP 500 «CT not running» настоящего PVE:
	// неоднозначным исходом (#275).
	stopRefusesStopped bool
}

func (f *managedFakePVE) NextID(context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.nextIDs) > 0 {
		id := f.nextIDs[0]
		f.nextIDs = f.nextIDs[1:]
		return id, nil
	}
	return 105, nil
}
func (f *managedFakePVE) Create(ctx context.Context, s proxmox.CTSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates++
	f.spec = s
	f.envs = append(f.envs, s.Env)
	f.exists = true
	f.digest++
	a, err := f.server.store.GetManagedActor(ctx, s.Owner.ActorUID)
	if err != nil || a.CTID != s.VMID || a.Stage != "create" || (a.Operation != "create" && a.Operation != "reset") || a.UPID != "" || a.Incarnation != int64(s.Owner.Incarnation) || a.OwnerNonce != s.Owner.Nonce {
		return "", errors.New("write before durable intent")
	}
	if f.uncertainWithUPID {
		return "create-task", proxmox.ErrUncertain
	}
	if f.uncertain {
		return "", proxmox.ErrUncertain
	}
	return "create-task", nil
}
func (f *managedFakePVE) Config(context.Context, int) (proxmox.CTConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.exists {
		return proxmox.CTConfig{}, proxmox.ErrNotFound
	}
	return proxmox.CTConfig{Description: proxmox.OwnedDescription(f.spec.Owner), Unprivileged: 1, Digest: fmt.Sprintf("%040x", f.digest)}, nil
}
func (f *managedFakePVE) InspectOwned(ctx context.Context, ref proxmox.OwnedCT) (proxmox.CTConfig, error) {
	if f.inspectStarted != nil {
		close(f.inspectStarted)
		<-f.inspectRelease
	}
	if f.inspectError != nil {
		return proxmox.CTConfig{}, f.inspectError
	}
	c, err := f.Config(ctx, ref.VMID)
	if err == nil && c.Description != proxmox.OwnedDescription(ref.Owner) {
		err = proxmox.ErrOwnership
	}
	return c, err
}
func (f *managedFakePVE) Status(context.Context, int) (proxmox.CTStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := "stopped"
	if f.running {
		s = "running"
	}
	return proxmox.CTStatus{Status: s}, nil
}
func (f *managedFakePVE) WaitTask(_ context.Context, upid string) error {
	f.mu.Lock()
	wait := f.stopWait
	f.mu.Unlock()
	if upid == "stop-task" {
		time.Sleep(wait)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.waited = append(f.waited, upid)
	return f.taskErrors[upid]
}
func (f *managedFakePVE) ConfirmAbsent(_ context.Context, id int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.absenceChecks++
	if f.absenceError != nil {
		return f.absenceError
	}
	if f.exists || id != f.spec.VMID {
		return proxmox.ErrOwnership
	}
	return nil
}
func (f *managedFakePVE) Start(ctx context.Context, r proxmox.OwnedCT) (string, error) {
	f.mu.Lock()
	before := f.beforeStart
	f.mu.Unlock()
	if before != nil {
		before()
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("PVE request canceled: %w", err)
	}
	if _, err := f.InspectOwned(ctx, r); err != nil {
		return "", err
	}
	f.mu.Lock()
	if f.startReadFailures > 0 {
		f.startReadFailures--
		f.mu.Unlock()
		return "", errors.New("PVE HTTPS request failed")
	}
	if f.startError != nil {
		err := f.startError
		f.mu.Unlock()
		return "", err
	}
	f.starts++
	f.running = true
	after := f.afterStart
	upid := "start-task"
	var err error
	switch {
	case f.uncertainStart:
		upid, err = "", proxmox.ErrUncertain
	case f.startUPID != "":
		upid = f.startUPID
	}
	f.mu.Unlock()
	if after != nil {
		after()
	}
	return upid, err
}
func (f *managedFakePVE) Stop(ctx context.Context, r proxmox.OwnedCT) (string, error) {
	if _, err := f.InspectOwned(ctx, r); err != nil {
		return "", err
	}
	f.mu.Lock()
	if f.stopReadFailures > 0 {
		f.stopReadFailures--
		f.mu.Unlock()
		return "", errors.New("PVE HTTPS request failed")
	}
	f.stops++
	if f.stopRefusesStopped && !f.running {
		f.mu.Unlock()
		return "", fmt.Errorf("%w: PVE HTTP status 500", proxmox.ErrUncertain)
	}
	f.running = false
	after := f.afterStop
	uncertain := f.uncertainStop
	f.mu.Unlock()
	if after != nil {
		after()
	}
	if uncertain {
		return "", proxmox.ErrUncertain
	}
	return "stop-task", nil
}
func (f *managedFakePVE) Delete(ctx context.Context, r proxmox.OwnedCT) (string, error) {
	f.mu.Lock()
	before := f.beforeDelete
	f.mu.Unlock()
	if before != nil {
		before()
	}
	f.mu.Lock()
	if f.deleteReadFailures > 0 {
		f.deleteReadFailures--
		f.mu.Unlock()
		return "", errors.New("PVE HTTPS request failed")
	}
	f.mu.Unlock()
	if _, err := f.InspectOwned(ctx, r); err != nil {
		return "", err
	}
	f.mu.Lock()
	f.deletes++
	if f.running {
		// PVE отвергает Delete работающей CT ответом HTTP 500, а клиент
		// считает 500 неоднозначным (#277).
		f.deletedWhileRunning = true
		f.mu.Unlock()
		return "", fmt.Errorf("%w: PVE HTTP status 500", proxmox.ErrUncertain)
	}
	f.exists = false
	after := f.afterDelete
	uncertain := f.uncertainDelete
	f.mu.Unlock()
	if after != nil {
		after()
	}
	if uncertain {
		return "", proxmox.ErrUncertain
	}
	return "delete-task", nil
}
func (f *managedFakePVE) ConfigureBootstrap(ctx context.Context, ref proxmox.OwnedCT, env proxmox.BootstrapEnv) error {
	a, err := f.server.store.GetManagedActor(ctx, ref.Owner.ActorUID)
	if err != nil {
		return err
	}
	if a.Operation != "start" || a.Stage != "start" || a.UPID != "" {
		return errors.New("bootstrap before durable start intent")
	}
	f.mu.Lock()
	if deadline, ok := ctx.Deadline(); ok {
		f.bootstrapBudgets = append(f.bootstrapBudgets, time.Until(deadline))
	}
	if f.bootstrapReadFailures > 0 {
		f.bootstrapReadFailures--
		f.mu.Unlock()
		return errors.New("PVE HTTPS request failed")
	}
	if !f.exists || f.running {
		f.mu.Unlock()
		return errors.New("bootstrap must configure the created CT while stopped")
	}
	f.bootstraps++
	f.spec.Env = env
	f.envs = append(f.envs, env)
	f.digest++
	uncertain := f.bootstrapUncertain > 0
	if uncertain {
		f.bootstrapUncertain--
	}
	during := f.duringBootstrap
	f.mu.Unlock()
	if during != nil {
		during()
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: PVE request canceled: %w", proxmox.ErrUncertain, err)
	}
	if uncertain {
		return fmt.Errorf("%w: PVE HTTP status 500", proxmox.ErrUncertain)
	}
	return f.bootstrapError
}
func (f *managedFakePVE) Close() {}

type managedFakeRelay struct {
	mu          sync.Mutex
	connected   bool
	authorize   func(string, int64, string) bool
	disconnects int
	dialError   error
}

func (h *managedFakeRelay) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) })
}
func (h *managedFakeRelay) HostKey() string { return "public-ssh-host-key" }
func (h *managedFakeRelay) Connected(string, int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.connected
}
func (h *managedFakeRelay) Dial(ctx context.Context, uid string, g int64) (net.Conn, error) {
	h.mu.Lock()
	err := h.dialError
	h.mu.Unlock()
	if err != nil {
		return nil, err
	}
	a, b := net.Pipe()
	go func() {
		defer b.Close()
		buf := make([]byte, 4096)
		b.Read(buf)
		io.WriteString(b, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
	}()
	return a, nil
}
func (h *managedFakeRelay) Disconnect(string, int64) { h.mu.Lock(); h.disconnects++; h.mu.Unlock() }
func (h *managedFakeRelay) Close() error             { return nil }

// Канал сообщений роя фейку не нужен: роутер проверяется своим тестом
// с подменённым deliverMessage, но интерфейс требует методов.
func (h *managedFakeRelay) SetMessageRouter(relay.MessageRouter) {}
func (h *managedFakeRelay) DialMessage(context.Context, string, int64, []byte) (net.Conn, error) {
	return nil, errors.New("swarm messages are not wired in the fake relay")
}

// managedSetup — сервер с managed-пулом brood-proxmox; configure правит
// настройки сборки, например подключает поддельный Bot API.
func managedSetup(t *testing.T, configure ...func(*Config)) (*substrateServer, *http.Client, *http.Client, string, *managedFakePVE, *managedFakeRelay) {
	t.Helper()
	cfg, err := loadProxmox(runtimeFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	pve := &managedFakePVE{}
	hub := &managedFakeRelay{}
	ts := newSubstrateServer(t, newFakeAteapi(), append([]func(*Config){func(c *Config) { c.proxmox = cfg.proxmox }}, configure...)...)
	ts.srv.managed.stopReconcile()
	ts.srv.managed.newClient = func(proxmox.Config) (proxmoxAPI, error) { return pve, nil }
	if ts.srv.managed.hub != nil {
		ts.srv.managed.hub.Close()
	}
	ts.srv.managed.hub = hub
	hub.authorize = ts.srv.managed.authorizeGuest
	pve.server = ts.srv
	admin, alice, bob, id := actorsSetup(t, ts.testServer)
	r, b, _ := ts.do(admin, "POST", "/api/proxmox-pools/brood-proxmox/activate", ts.csrf(admin), nil)
	expectStatus(t, "activate", r, b, 201)
	return ts, alice, bob, id, pve, hub
}

func TestManagedActorDurableLifecycleAndACL(t *testing.T) {
	ts, alice, bob, id, pve, hub := managedSetup(t)
	path := "/api/swarms/" + id + "/actors"
	body := map[string]any{"metadata": map[string]string{"name": "coding"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}}
	r, b, _ := ts.do(alice, "POST", path, ts.csrf(alice), body)
	expectStatus(t, "managed create", r, b, 201)
	uid := b["metadata"].(map[string]any)["uid"].(string)
	if b["provider"] != "proxmox" || b["workerAssignment"] != nil {
		t.Fatalf("fake native identity: %#v", b)
	}
	for range 3 {
		ts.srv.managed.reconcileOnce(context.Background())
	}
	r, b, _ = ts.do(alice, "GET", path+"/coding", "", nil)
	expectStatus(t, "read", r, b, 200)
	if b["status"].(map[string]any)["state"] == "ACTOR_STATE_RUNNING" {
		t.Fatal("task-only readiness marked actor running")
	}
	hub.mu.Lock()
	hub.connected = true
	hub.mu.Unlock()
	ts.srv.managed.reconcileOnce(context.Background())
	r, b, _ = ts.do(alice, "GET", path+"/coding", "", nil)
	expectStatus(t, "ready", r, b, 200)
	if b["status"].(map[string]any)["state"] != "ACTOR_STATE_RUNNING" {
		t.Fatalf("ready actor not running: %#v", b)
	}
	r, b, _ = ts.do(bob, "GET", path+"/coding", "", nil)
	expectStatus(t, "foreign actor", r, b, 404)
	r, b, _ = ts.do(alice, "POST", path+"/coding", ts.csrf(alice), map[string]string{"action": "suspend"})
	expectStatus(t, "unsupported checkpoint", r, b, 409)
	a, err := ts.srv.store.GetManagedActor(context.Background(), uid)
	if err != nil {
		t.Fatal(err)
	}
	token := pve.spec.Env["BROOD_GUEST_RELAY_TOKEN"]
	if token == "" || !hub.authorize(uid, a.Incarnation, token) || hub.authorize(uid, a.Incarnation+1, token) {
		t.Fatal("guest token not incarnation scoped")
	}
	for key, value := range pve.spec.Env {
		if !strings.HasPrefix(key, "BROOD_GUEST_") || strings.Contains(value, proxmoxSentinel) || strings.Contains(value, strings.Repeat("r", 32)) {
			t.Fatal("operator credentials in guest environment")
		}
	}
	if pve.bootstraps != 1 {
		t.Fatalf("start omitted digest-CAS guest bootstrap: %d", pve.bootstraps)
	}
	if pve.creates != 1 || pve.starts != 1 {
		t.Fatalf("duplicate execution: creates=%d starts=%d", pve.creates, pve.starts)
	}
	r, b, _ = ts.do(alice, "DELETE", "/api/swarms/"+id, ts.csrf(alice), nil)
	expectStatus(t, "managed child guard", r, b, 409)
	r, b, _ = ts.do(alice, "POST", path+"/coding", ts.csrf(alice), map[string]string{"action": "stop"})
	expectStatus(t, "stop intent", r, b, 200)
	for range 2 {
		ts.srv.managed.reconcileOnce(context.Background())
	}
	a, _ = ts.srv.store.GetManagedActor(context.Background(), uid)
	if a.Phase != "stopped" || hub.authorize(uid, a.Incarnation, token) {
		t.Fatalf("stopped allocation/relay wrong: %+v", a)
	}
	r, b, _ = ts.do(alice, "POST", path+"/coding", ts.csrf(alice), map[string]string{"action": "resume"})
	expectStatus(t, "resume", r, b, 200)
	for range 2 {
		ts.srv.managed.reconcileOnce(context.Background())
	}
	if pve.creates != 1 {
		t.Fatal("resume replaced persistent workspace CT")
	}
}

// FEATURE: Exact root-supervisor guest bootstrap contract.
// SPEC: Required public allowlist and CA key are fixed by the provider, never
// selected by actors; only scoped relay credentials enter the guest.
func TestManagedGuestBootstrapCommandContract(t *testing.T) {
	ts, alice, _, id, pve, _ := managedSetup(t)
	ts.srv.managed.publicCA = "public certificate PEM\n"
	r, b, _ := ts.do(alice, "POST", "/api/swarms/"+id+"/actors", ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "contract"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}})
	expectStatus(t, "create", r, b, 201)
	ts.srv.managed.reconcileOnce(context.Background())
	env := pve.spec.Env
	keys := []string{"BROOD_GUEST_ACTOR_UID", "BROOD_GUEST_GENERATION", "BROOD_GUEST_RELAY_URL", "BROOD_GUEST_RELAY_TOKEN", "BROOD_GUEST_RELAY_HOST_KEY", "BROOD_GUEST_CA_PEM_B64", "BROOD_GUEST_ALLOWED_HOSTS"}
	if len(env) != len(keys) {
		t.Fatalf("wrong guest key set: %v", env)
	}
	for _, key := range keys {
		if env[key] == "" {
			t.Errorf("required guest key missing: %s", key)
		}
	}
	if env["BROOD_GUEST_CA_PEM_B64"] != base64.StdEncoding.EncodeToString([]byte(ts.srv.managed.publicCA)) {
		t.Error("public CA not encoded under the actual guest command key")
	}
	var hosts []string
	if err := json.Unmarshal([]byte(env["BROOD_GUEST_ALLOWED_HOSTS"]), &hosts); err != nil {
		t.Fatalf("required host policy is not JSON: %v", err)
	}
	for _, required := range []string{"api.anthropic.com", "github.com", "api.github.com", "gitlab.com", "registry.npmjs.org", "nodejs.org", "bun.com", "pypi.org", "files.pythonhosted.org", "proxy.golang.org", "sum.golang.org", "crates.io", "static.crates.io", "sh.rustup.rs"} {
		found := false
		for _, host := range hosts {
			found = found || host == required
			if strings.ContainsAny(host, "*/:") || strings.HasSuffix(host, ".invalid") || strings.HasSuffix(host, ".local") {
				t.Fatalf("nonpublic or nonexact policy host: %q", host)
			}
		}
		if !found {
			t.Errorf("legitimate CLI/developer host missing: %s", required)
		}
	}
	if !reflect.DeepEqual(env, ts.srv.managed.createSpec(mustManagedActor(t, ts, id)).Env) {
		t.Fatal("guest bootstrap policy is nondeterministic")
	}
}

func mustManagedActor(t *testing.T, ts *substrateServer, swarm string) store.ManagedActor {
	t.Helper()
	rows, err := ts.srv.store.ListManagedActors(context.Background(), swarm)
	if err != nil || len(rows) != 1 {
		t.Fatalf("expected one durable actor: %v %v", rows, err)
	}
	return rows[0]
}

// FEATURE: Staged destructive cleanup and replacement with continuous capacity.
// SPEC: Complete stop before delete; only positive global absence releases or
// rotates ownership, and prior task evidence must survive cleanup requests.
func TestManagedDeleteRequiresStopTaskAndGlobalAbsence(t *testing.T) {
	ts, alice, _, id, pve, hub := managedSetup(t)
	path := "/api/swarms/" + id + "/actors"
	r, b, _ := ts.do(alice, "POST", path, ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "cleanup"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}})
	expectStatus(t, "create", r, b, 201)
	hub.connected = true
	for range 3 {
		ts.srv.managed.reconcileOnce(context.Background())
	}
	before := mustManagedActor(t, ts, id)
	pve.absenceError = proxmox.ErrPermission
	r, b, _ = ts.do(alice, "DELETE", path+"/cleanup", ts.csrf(alice), nil)
	expectStatus(t, "cleanup intent", r, b, 202)
	for range 4 {
		ts.srv.managed.reconcileOnce(context.Background())
	}
	a := mustManagedActor(t, ts, id)
	if pve.stops != 1 || pve.deletes != 1 || pve.deletedWhileRunning || a.Phase == "deleted" || a.OwnerNonce != before.OwnerNonce || a.Incarnation != before.Incarnation {
		t.Fatalf("unsafe cleanup: actor=%+v stop=%d delete=%d runningDelete=%v", a, pve.stops, pve.deletes, pve.deletedWhileRunning)
	}
	if pve.absenceChecks == 0 {
		t.Fatal("no global absence proof requested")
	}
	p, _ := ts.srv.store.GetManagedPool(context.Background(), a.PoolUID)
	usage, _ := ts.srv.managed.poolJSON(context.Background(), p)
	if usage.UsedActors != "1" {
		t.Fatal("uncertain absence released capacity")
	}
	for _, refusal := range []error{proxmox.ErrUncertain, proxmox.ErrOwnership} {
		pve.absenceError = refusal
		ts.srv.managed.reconcileOnce(context.Background())
		if current := mustManagedActor(t, ts, id); current.Phase == "deleted" || pve.deletes != 1 {
			t.Fatal("failed global proof released capacity or repeated delete")
		}
	}
	pve.absenceError = nil
	ts.srv.managed.reconcileOnce(context.Background())
	a = mustManagedActor(t, ts, id)
	if a.Phase != "deleted" || pve.deletes != 1 {
		t.Fatalf("confirmed cleanup not retired exactly once: %+v deletes=%d", a, pve.deletes)
	}
	usage, _ = ts.srv.managed.poolJSON(context.Background(), p)
	if usage.UsedActors != "0" {
		t.Fatal("confirmed deletion did not release capacity")
	}
	r, b, _ = ts.do(alice, "GET", path, "", nil)
	expectStatus(t, "list", r, b, 200)
	if len(b["results"].([]any)) != 0 {
		t.Fatal("deleted tombstone leaked into inventory")
	}
}

func TestManagedCleanupWaitsForTaskProofDespiteStoppedOrAbsentCT(t *testing.T) {
	for _, lostReply := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending stop task", true: "lost delete response"}[lostReply], func(t *testing.T) {
			ts, alice, _, id, pve, hub := managedSetup(t)
			path := "/api/swarms/" + id + "/actors"
			r, b, _ := ts.do(alice, "POST", path, ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "task-proof"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}})
			expectStatus(t, "create", r, b, 201)
			hub.connected = true
			for range 3 {
				ts.srv.managed.reconcileOnce(context.Background())
			}
			pve.uncertainDelete = lostReply
			if !lostReply {
				pve.taskErrors = map[string]error{"stop-task": proxmox.ErrUncertain}
			}
			r, b, _ = ts.do(alice, "DELETE", path+"/task-proof", ts.csrf(alice), nil)
			expectStatus(t, "cleanup intent", r, b, 202)
			for range 4 {
				ts.srv.managed.reconcileOnce(context.Background())
			}
			a := mustManagedActor(t, ts, id)
			if a.Phase == "deleted" || pve.stops != 1 || (!lostReply && pve.deletes != 0) || (lostReply && (pve.deletes != 1 || a.UPID != "")) || pve.absenceChecks != 0 {
				t.Fatalf("CT state bypassed pending task proof: phase=%s stage=%s stops=%d deletes=%d absenceChecks=%d", a.Phase, a.Stage, pve.stops, pve.deletes, pve.absenceChecks)
			}
			if !lostReply {
				pve.taskErrors = nil
				for range 3 {
					ts.srv.managed.reconcileOnce(context.Background())
				}
				if a = mustManagedActor(t, ts, id); a.Phase != "deleted" || pve.deletes != 1 {
					t.Fatal("terminal stop proof did not allow exactly one delete")
				}
			}
		})
	}
}

func TestManagedResetPreservesOldOwnershipUntilCleanup(t *testing.T) {
	ts, alice, _, id, pve, hub := managedSetup(t)
	path := "/api/swarms/" + id + "/actors"
	r, b, _ := ts.do(alice, "POST", path, ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "reset"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}})
	expectStatus(t, "create", r, b, 201)
	hub.connected = true
	for range 3 {
		ts.srv.managed.reconcileOnce(context.Background())
	}
	before := mustManagedActor(t, ts, id)
	oldToken := pve.spec.Env["BROOD_GUEST_RELAY_TOKEN"]
	pve.absenceError = proxmox.ErrPermission
	r, b, _ = ts.do(alice, "POST", path+"/reset", ts.csrf(alice), map[string]string{"action": "reset"})
	expectStatus(t, "destructive reset intent", r, b, 200)
	for range 4 {
		ts.srv.managed.reconcileOnce(context.Background())
	}
	a := mustManagedActor(t, ts, id)
	if a.OwnerNonce != before.OwnerNonce || a.Incarnation != before.Incarnation || pve.creates != 1 || a.Phase == "deleted" {
		t.Fatalf("reset rotated before global cleanup: %+v", a)
	}
	if hub.authorize(a.UID, a.Incarnation, oldToken) {
		t.Fatal("reset cleanup still authorizes old guest")
	}
	pve.absenceError = nil
	for range 5 {
		ts.srv.managed.reconcileOnce(context.Background())
	}
	a = mustManagedActor(t, ts, id)
	if a.UID != before.UID || a.CTID != before.CTID || a.Resources != before.Resources || a.OwnerNonce == before.OwnerNonce || a.Incarnation != before.Incarnation+1 || pve.creates != 2 || pve.deletes != 1 || a.Phase != "running" {
		t.Fatalf("reset did not replace exact allocation: %+v creates=%d deletes=%d", a, pve.creates, pve.deletes)
	}
	if hub.authorize(a.UID, before.Incarnation, oldToken) || pve.spec.Env["BROOD_GUEST_RELAY_TOKEN"] == oldToken {
		t.Fatal("reset reused old generation authority")
	}
	p, _ := ts.srv.store.GetManagedPool(context.Background(), a.PoolUID)
	usage, _ := ts.srv.managed.poolJSON(context.Background(), p)
	if usage.UsedActors != "1" {
		t.Fatal("reset lost retained reservation")
	}
}

func TestManagedDeleteReconcilesPriorCreateTaskBeforeCleanup(t *testing.T) {
	for _, taskErr := range []error{nil, proxmox.ErrTaskFailed} {
		t.Run(map[bool]string{true: "failed terminal create", false: "successful create"}[taskErr != nil], func(t *testing.T) {
			ts, alice, _, id, pve, _ := managedSetup(t)
			pve.taskErrors = map[string]error{"create-task": taskErr}
			path := "/api/swarms/" + id + "/actors"
			r, b, _ := ts.do(alice, "POST", path, ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "early-delete"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}})
			expectStatus(t, "create", r, b, 201)
			r, b, _ = ts.do(alice, "DELETE", path+"/early-delete", ts.csrf(alice), nil)
			expectStatus(t, "safe delete after terminal prior task", r, b, 202)
			for range 4 {
				ts.srv.managed.reconcileOnce(context.Background())
			}
			a := mustManagedActor(t, ts, id)
			if a.Phase != "deleted" || pve.starts != 0 || pve.deletes != 1 || pve.absenceChecks == 0 {
				t.Fatalf("early cleanup incomplete or started guest: phase=%s starts=%d deletes=%d", a.Phase, pve.starts, pve.deletes)
			}
			if len(pve.waited) == 0 || pve.waited[0] != "create-task" {
				t.Fatal("cleanup replaced prior create UPID before task proof")
			}
		})
	}
}

func TestManagedUncertainStartAndUnfinishedTaskBlockMutations(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending create task", true: "lost start reply"}[uncertain], func(t *testing.T) {
			ts, alice, _, id, pve, hub := managedSetup(t)
			if !uncertain {
				pve.taskErrors = map[string]error{"create-task": proxmox.ErrUncertain}
			}
			pve.uncertainStart = uncertain
			path := "/api/swarms/" + id + "/actors"
			r, b, _ := ts.do(alice, "POST", path, ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "pending"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}})
			expectStatus(t, "create", r, b, 201)
			hub.connected = true
			for range 3 {
				ts.srv.managed.reconcileOnce(context.Background())
			}
			before := mustManagedActor(t, ts, id)
			for _, action := range []string{"stop", "reset", "delete"} {
				method := "POST"
				var body any = map[string]string{"action": action}
				if action == "delete" {
					method = "DELETE"
					body = nil
				}
				r, b, _ = ts.do(alice, method, path+"/pending", ts.csrf(alice), body)
				expectStatus(t, "reject unreconciled "+action, r, b, 409)
			}
			for range 3 {
				ts.srv.managed.reconcileOnce(context.Background())
			}
			a := mustManagedActor(t, ts, id)
			if a.OperationID != before.OperationID || a.UPID != before.UPID || a.OwnerNonce != before.OwnerNonce || pve.creates != 1 || pve.starts > 1 || pve.stops != 0 || pve.deletes != 0 {
				t.Fatalf("pending write evidence replaced: %+v", a)
			}
		})
	}
}

// FEATURE: Uncertain responses preserve every piece of provider evidence.
// SPEC: Never drop a returned UPID alongside an error and never start an
// unconfigured guest. A synchronous bootstrap whose response was lost is not a
// start: it is written again under the digest CAS before the one Start (#275).
func TestManagedUncertainResponseRetainsReturnedUPID(t *testing.T) {
	ts, alice, _, id, pve, _ := managedSetup(t)
	pve.uncertainWithUPID = true
	r, b, _ := ts.do(alice, "POST", "/api/swarms/"+id+"/actors", ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "evidence"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}})
	expectStatus(t, "durable uncertain create", r, b, 201)
	if a := mustManagedActor(t, ts, id); a.UPID != "create-task" {
		t.Fatal("returned task evidence dropped on uncertain response")
	}
}

func TestManagedTaskEvidencePersistsAfterRequestCancellation(t *testing.T) {
	ts, alice, _, id, _, _ := managedSetup(t)
	r, b, _ := ts.do(alice, "POST", "/api/swarms/"+id+"/actors", ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "canceled"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}})
	expectStatus(t, "create", r, b, 201)
	a := mustManagedActor(t, ts, id)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	saved, err := ts.srv.managed.recordTask(ctx, a, a.UPID, proxmox.ErrUncertain)
	if err != nil || saved.UPID != a.UPID || saved.Phase != "error" {
		t.Fatalf("provider reply lost when browser disconnected: phase=%s err=%v", saved.Phase, err)
	}
}

// TestManagedUncertainBootstrapIsRetriedBeforeStart: запись bootstrap env,
// чей ответ потерян, — не Start (#275, как start update в #133). CT
// проверена остановленной и своей, запись идёт через digest CAS с тем же
// содержимым, поэтому каждый проход пишет env снова, а Start не уходит, пока
// PVE не подтвердит запись; после неё CT запускается ровно один раз.
func TestManagedUncertainBootstrapIsRetriedBeforeStart(t *testing.T) {
	ts, alice, _, id, pve, hub := managedSetup(t)
	pve.bootstrapError = proxmox.ErrUncertain
	r, b, _ := ts.do(alice, "POST", "/api/swarms/"+id+"/actors", ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "bootstrap"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}})
	expectStatus(t, "create", r, b, 201)
	for range 5 {
		ts.srv.managed.reconcileOnce(context.Background())
	}
	a := mustManagedActor(t, ts, id)
	if pve.bootstraps != 5 || pve.starts != 0 || a.Operation != "start" || a.Stage != "start" || a.UPID != "" || a.Phase != "stopped" || a.Error != managedProviderError(errBootstrapUnconfirmed) {
		t.Fatalf("unconfirmed bootstrap not retried before Start: bootstraps=%d starts=%d %+v", pve.bootstraps, pve.starts, a)
	}
	pve.mu.Lock()
	pve.bootstrapError = nil
	pve.mu.Unlock()
	hub.mu.Lock()
	hub.connected = true
	hub.mu.Unlock()
	for range 4 {
		ts.srv.managed.reconcileOnce(context.Background())
	}
	a = mustManagedActor(t, ts, id)
	if !actorRunning(a) || pve.bootstraps != 6 || pve.starts != 1 {
		t.Fatalf("confirmed bootstrap did not start the guest exactly once: bootstraps=%d starts=%d %+v", pve.bootstraps, pve.starts, a)
	}
}

// BUG: Терминальные ошибки stop/delete навсегда блокируют явный повтор.
// HYPOTHESIS: Runtime не закрывает failed task slot, а API принимает desired за успех.
// SOLUTION: Fenced ResolveManagedActorTaskFailure сохраняет proof; явный повтор получает новый ID.
// RESULT: FIXED — все пять вариантов cleanup завершаются без автоматического replay.
// STATUS: FIXED
func TestManagedTerminalFailureExplicitRetry(t *testing.T) {
	for _, action := range []string{"stop", "delete", "reset"} {
		for _, stage := range []string{"stop", "delete"} {
			if action == "stop" && stage == "delete" {
				continue
			}
			t.Run(action+"/"+stage, func(t *testing.T) {
				ts, alice, _, id, pve, hub := managedSetup(t)
				path := "/api/swarms/" + id + "/actors"
				r, b, _ := ts.do(alice, "POST", path, ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "retry"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}})
				expectStatus(t, "create", r, b, 201)
				hub.connected = true
				for range 3 {
					ts.srv.managed.reconcileOnce(context.Background())
				}
				method := "POST"
				var body any = map[string]string{"action": action}
				if action == "delete" {
					method, body = "DELETE", nil
				}
				r, b, _ = ts.do(alice, method, path+"/retry", ts.csrf(alice), body)
				expectStatus(t, "initial intent", r, b, map[bool]int{true: 202, false: 200}[action == "delete"])
				if stage == "delete" {
					ts.srv.managed.reconcileOnce(context.Background())
				}
				before := mustManagedActor(t, ts, id)
				pve.exists, pve.running = true, stage == "stop"
				pve.taskErrors = map[string]error{before.UPID: proxmox.ErrTaskFailed}
				for range 3 {
					ts.srv.managed.reconcileOnce(context.Background())
				}
				a := mustManagedActor(t, ts, id)
				if a.Stage != "done" || a.Phase != "error" || a.Error == "" || a.UPID != before.UPID || a.OperationID != before.OperationID || a.OwnerNonce != before.OwnerNonce || a.Incarnation != before.Incarnation || a.Resources != before.Resources {
					t.Fatalf("terminal failed %s not durably reconciled: stage=%s phase=%s task=%s", stage, a.Stage, a.Phase, a.UPID)
				}
				pool, _ := ts.srv.store.GetManagedPool(context.Background(), a.PoolUID)
				usage, _ := ts.srv.managed.poolJSON(context.Background(), pool)
				if usage.UsedActors != "1" {
					t.Fatal("failed task released reservation")
				}
				stops, deletes, waits := pve.stops, pve.deletes, len(pve.waited)
				for range 3 {
					ts.srv.managed.reconcileOnce(context.Background())
				}
				if pve.stops != stops || pve.deletes != deletes || len(pve.waited) != waits {
					t.Fatal("completed failed task automatically replayed or polled")
				}
				pve.taskErrors = map[string]error{before.UPID: proxmox.ErrNotFound}
				r, b, _ = ts.do(alice, method, path+"/retry", ts.csrf(alice), body)
				expectStatus(t, "explicit retry", r, b, map[bool]int{true: 202, false: 200}[action == "delete"])
				retried := mustManagedActor(t, ts, id)
				// Упавшая delete-задача оставила CT остановленной: явный повтор
				// delete и reset не шлёт ей vzstop (#275), а сразу получает
				// свежий слот delete.
				if stage == "delete" {
					if retried.OperationID == before.OperationID || retried.Stage != "delete" || retried.UPID != "delete-task" || pve.stops != stops || pve.deletes != deletes+1 {
						t.Fatalf("explicit %s retry did not allocate a fresh delete slot for the owned stopped CT: %+v stops=%d->%d", action, retried, stops, pve.stops)
					}
				} else if retried.OperationID == before.OperationID || retried.Stage != "stop" || pve.stops != stops+1 {
					t.Fatal("explicit retry did not allocate a fresh stop task slot")
				}
				if _, err := ts.srv.managed.reconcileActor(context.Background(), before); !errors.Is(err, store.ErrRevisionConflict) {
					t.Fatalf("stale failed callback not fenced: %v", err)
				}
				pve.taskErrors = nil
				for range 5 {
					ts.srv.managed.reconcileOnce(context.Background())
				}
				a = mustManagedActor(t, ts, id)
				want := map[string]string{"stop": "stopped", "delete": "deleted", "reset": "running"}[action]
				if a.Phase != want {
					t.Fatalf("retry did not finish: %+v", a)
				}
			})
		}
	}
}

// BUG: Scoped config read скрывает глобально отсутствующий CT.
// HYPOTHESIS: Только config 404 разрешает authoritative absence probe.
// SOLUTION: Любой отказ read допускает положительный global proof, иначе сохраняется исходный отказ.
// RESULT: FIXED — ошибки 403/500/ownership/network сами по себе не доказывают отсутствие.
// STATUS: FIXED
func TestManagedObservationRequiresPositiveGlobalAbsence(t *testing.T) {
	for _, inspectErr := range []error{proxmox.ErrNotFound, proxmox.ErrPermission, proxmox.ErrOwnership, proxmox.ErrUncertain, errors.New("HTTP 500")} {
		for _, absent := range []bool{false, true} {
			t.Run(inspectErr.Error()+"/"+map[bool]string{true: "absent", false: "unproven"}[absent], func(t *testing.T) {
				f := &managedFakePVE{inspectError: inspectErr, spec: proxmox.CTSpec{VMID: 105}}
				if !absent {
					f.absenceError = proxmox.ErrUncertain
				}
				state, err := managedObservedState(context.Background(), f, store.ManagedActor{CTID: 105})
				if absent && (err != nil || state != "absent") {
					t.Fatalf("positive absence ignored: state=%s err=%v", state, err)
				}
				if !absent && (!errors.Is(err, inspectErr) || state != "") {
					t.Fatalf("original read failure lost: %v", err)
				}
			})
		}
	}
}

// BUG: Завершённый start блокирует recovery при недоступном ttyd.
// HYPOTHESIS: Закрытие task slot ошибочно зависит от готовности терминала.
// SOLUTION: Task завершается отдельно от readiness; timeout диагностирует отказ, Stop/Resume доступен.
// RESULT: FIXED — нет ложного running, после настоящего ttyd probe recovery завершается.
// STATUS: FIXED
func TestManagedReadinessSettlesTaskWithoutPublishingRunning(t *testing.T) {
	ts, alice, _, id, pve, hub := managedSetup(t)
	hub.connected, hub.dialError = true, errors.New("ttyd is not accepting connections")
	path := "/api/swarms/" + id + "/actors"
	r, b, _ := ts.do(alice, "POST", path, ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "not-ready"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}})
	expectStatus(t, "create", r, b, 201)
	for range 3 {
		ts.srv.managed.reconcileOnce(context.Background())
	}
	a := mustManagedActor(t, ts, id)
	if a.Stage != "done" || a.Phase == "running" {
		t.Fatalf("readiness blocked completed task or faked running: stage=%s phase=%s", a.Stage, a.Phase)
	}
	pve.taskErrors = map[string]error{a.UPID: proxmox.ErrNotFound}
	// Recovery must not consult expired task logs, including a fresh API intent.
	r, b, _ = ts.do(alice, "POST", path+"/not-ready", ts.csrf(alice), map[string]string{"action": "stop"})
	expectStatus(t, "stop without ready ttyd", r, b, 200)
	for range 2 {
		ts.srv.managed.reconcileOnce(context.Background())
	}
	r, b, _ = ts.do(alice, "POST", path+"/not-ready", ts.csrf(alice), map[string]string{"action": "resume"})
	expectStatus(t, "resume recovery", r, b, 200)
	pve.taskErrors = nil
	for range 2 {
		ts.srv.managed.reconcileOnce(context.Background())
	}
	a = mustManagedActor(t, ts, id)
	ts.srv.managed.readiness.Store(a.UID, managedReadiness{a.OperationID, time.Now().Add(-3 * time.Minute)})
	ts.srv.managed.reconcileOnce(context.Background())
	a = mustManagedActor(t, ts, id)
	if a.Stage != "done" || a.Phase != "error" || a.Error != managedReadinessError {
		t.Fatalf("never-ready CT did not get bounded diagnostic: %+v", a)
	}
	r, b, _ = ts.do(alice, "POST", path+"/not-ready", ts.csrf(alice), map[string]string{"action": "stop"})
	expectStatus(t, "stop after readiness timeout", r, b, 200)
	for range 2 {
		ts.srv.managed.reconcileOnce(context.Background())
	}
	r, b, _ = ts.do(alice, "POST", path+"/not-ready", ts.csrf(alice), map[string]string{"action": "resume"})
	expectStatus(t, "resume after readiness timeout", r, b, 200)
	hub.mu.Lock()
	hub.dialError = nil
	hub.mu.Unlock()
	for range 2 {
		ts.srv.managed.reconcileOnce(context.Background())
	}
	if a = mustManagedActor(t, ts, id); a.Phase != "running" || a.Error != "" {
		t.Fatalf("real ttyd recovery did not restore readiness: %+v", a)
	}
}

// BUG: Неизвестный исход нельзя считать терминальной ошибкой по состоянию CT.
// HYPOTHESIS: Только ErrTaskFailed и независимая проверка владения закрывают slot.
func TestManagedFailureResolutionRequiresTerminalAndOwnedProof(t *testing.T) {
	for _, stage := range []string{"stop", "delete"} {
		for _, tc := range []struct {
			name                            string
			taskErr, inspectErr, absenceErr error
			absent, done                    bool
		}{
			{"unknown", proxmox.ErrUncertain, nil, nil, true, false},
			{"canceled", context.Canceled, nil, nil, true, false},
			{"network", errors.New("connection reset"), nil, nil, true, false},
			{"expired-pending-log", proxmox.ErrNotFound, nil, nil, true, false},
			{"foreign", proxmox.ErrTaskFailed, proxmox.ErrOwnership, proxmox.ErrOwnership, false, false},
			{"permission", proxmox.ErrTaskFailed, proxmox.ErrPermission, proxmox.ErrPermission, true, false},
			{"positive-global-absence", proxmox.ErrTaskFailed, proxmox.ErrPermission, nil, true, true},
		} {
			t.Run(stage+"/"+tc.name, func(t *testing.T) {
				ts, alice, _, id, pve, hub := managedSetup(t)
				path := "/api/swarms/" + id + "/actors"
				r, b, _ := ts.do(alice, "POST", path, ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "proof"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}})
				expectStatus(t, "create", r, b, 201)
				hub.connected = true
				for range 3 {
					ts.srv.managed.reconcileOnce(context.Background())
				}
				r, b, _ = ts.do(alice, "DELETE", path+"/proof", ts.csrf(alice), nil)
				expectStatus(t, "delete", r, b, 202)
				if stage == "delete" {
					ts.srv.managed.reconcileOnce(context.Background())
				}
				before := mustManagedActor(t, ts, id)
				pve.taskErrors = map[string]error{before.UPID: tc.taskErr}
				pve.inspectError, pve.absenceError = tc.inspectErr, tc.absenceErr
				pve.exists = !tc.absent
				for range 3 {
					ts.srv.managed.reconcileOnce(context.Background())
				}
				a := mustManagedActor(t, ts, id)
				want := stage
				if tc.done {
					want = "done"
				}
				if a.Stage != want || a.UPID != before.UPID || a.OperationID != before.OperationID || a.OwnerNonce != before.OwnerNonce || a.Incarnation != before.Incarnation || a.Phase != "error" || pve.stops != 1 || pve.deletes != map[string]int{"stop": 0, "delete": 1}[stage] {
					t.Fatalf("task proof lost or replayed: stage=%s phase=%s stops=%d deletes=%d", a.Stage, a.Phase, pve.stops, pve.deletes)
				}
				if !errors.Is(tc.taskErr, proxmox.ErrTaskFailed) && pve.absenceChecks != 0 {
					t.Fatal("CT absence used without terminal task proof")
				}
				if tc.done {
					checks := pve.absenceChecks
					r, b, _ = ts.do(alice, "DELETE", path+"/proof", ts.csrf(alice), nil)
					expectStatus(t, "retry absent delete", r, b, 202)
					a = mustManagedActor(t, ts, id)
					if a.Phase != "deleted" || a.OperationID == before.OperationID || pve.stops != 1 || pve.deletes != map[string]int{"stop": 0, "delete": 1}[stage] || pve.absenceChecks != checks+1 {
						t.Fatal("absent retry resubmitted cleanup or repeated positive global proof")
					}
				}
			})
		}
	}
}

func TestManagedUncertainCreateRetainsExactAllocation(t *testing.T) {
	ts, alice, _, id, pve, _ := managedSetup(t)
	pve.uncertain = true
	path := "/api/swarms/" + id + "/actors"
	r, b, _ := ts.do(alice, "POST", path, ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "uncertain"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}})
	expectStatus(t, "durable create", r, b, 201)
	for range 4 {
		ts.srv.managed.reconcileOnce(context.Background())
	}
	rows, err := ts.srv.store.ListManagedActors(context.Background(), id)
	if err != nil || len(rows) != 1 {
		t.Fatalf("allocation lost: %v", err)
	}
	a := rows[0]
	if a.CTID != 105 || a.Phase == "deleted" || pve.creates != 1 {
		t.Fatalf("uncertain creation duplicated or released: %+v creates %d", a, pve.creates)
	}
	if _, err = ts.srv.store.ReserveManagedActor(context.Background(), store.ManagedActor{}); err == nil {
		t.Fatal("store accepted invalid identity")
	}
}
