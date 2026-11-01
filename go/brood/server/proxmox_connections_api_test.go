package server

// FEATURE: Админский API подключений Proxmox (#118).
// SPEC: POST/GET/DELETE /api/proxmox-connections: валидация как у env-конфигурации,
// секрет write-only (не возвращается ни при создании, ни в списке), не-админ
// и аноним получают отказ; подключение попадает в список пулов как read-only
// до активации.
import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/store"
)

// FEATURE: Подключение из формы обслуживает управляемый runtime после рестарта.
// SPEC: Те же ограничения, что у env; строгий JSON; секрет только для PVE;
// удаление активного подключения и коллизии env/DB запрещены.
// IMPLEMENTATION: Runtime читается из SQLite через общий configuredPool/client.
// RESULT: IMPLEMENTED — activation, restart, lifecycle и cleanup проверены фейком.
// STATUS: DONE
func TestProxmoxConnectionRuntime(t *testing.T) {
	env := runtimeFixture(t)
	delete(env, proxmoxPoolsSetting)
	delete(env, proxmoxTokensSetting)
	cfg, err := loadProxmox(env)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBPath = t.TempDir() + "/runtime.db"
	ts := newSubstrateServer(t, newFakeAteapi(), func(c *Config) { c.proxmox = cfg.proxmox; c.DBPath = cfg.DBPath })
	ts.srv.managed.stopReconcile()
	admin, alice, _, swarmID := actorsSetup(t, ts.testServer)
	_, ca := selfSignFake(t, "form-runtime")
	body := connectionBody("form-runtime", string(ca))
	body["template"] = "local:vztmpl/brood-claude-proxmox-aaaaaaaa.tar.zst"
	body["templateDigest"] = "sha256:" + strings.Repeat("a", 64)
	body["capacity"] = map[string]any{"actors": 2, "cpuMillicores": "2000", "memoryBytes": "2147483648", "diskGiB": 8}
	r, b, _ := ts.do(admin, "POST", "/api/proxmox-connections", ts.csrf(admin), body)
	expectStatus(t, "create runtime", r, b, 201)
	uid := b["uid"].(string)
	r, b, _ = ts.do(admin, "GET", "/api/proxmox-pools", "", nil)
	expectStatus(t, "inventory", r, b, 200)
	if items := b["items"].([]any); len(items) != 1 || items[0].(map[string]any)["runtimeAvailable"] != true {
		t.Fatalf("runtime unavailable: %v", b)
	}
	r, b, _ = ts.do(admin, "POST", "/api/proxmox-pools/form-runtime/activate", ts.csrf(admin), nil)
	expectStatus(t, "activate", r, b, 201)
	p, err := ts.srv.managed.poolByName(context.Background(), "form-runtime")
	if err != nil {
		t.Fatal(err)
	}
	fake := &managedFakePVE{server: ts.srv}
	// Новый provider и новый handle БД не имеют process-local конфигурации.
	reopened, err := store.Open(context.Background(), cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	m := &managedProvider{server: &Server{store: reopened}, registry: cfg.proxmox}
	if err := m.initialize(cfg); err != nil {
		t.Fatal(err)
	}
	defer m.close()
	m.newClient = func(c proxmox.Config) (proxmoxAPI, error) {
		if c.Endpoint != body["endpoint"] || c.TokenSecret != body["tokenSecret"] || c.CAPEM != body["caPEM"] {
			t.Fatal("wrong persisted PVE client configuration")
		}
		return fake, nil
	}
	if client, err := m.client(p); err != nil || client != fake {
		t.Fatalf("restart client: %v", err)
	}
	r, b, _ = ts.do(admin, "DELETE", "/api/proxmox-connections/"+uid, ts.csrf(admin), nil)
	expectStatus(t, "active connection deletion", r, b, 409)

	// API актора использует сохранённый PVE credential, а не env pools.
	ts.srv.managed.newClient = m.newClient
	ts.srv.managed.hub.Close()
	hub := &managedFakeRelay{connected: true}
	hub.authorize = ts.srv.managed.authorizeGuest
	ts.srv.managed.hub = hub
	path := "/api/swarms/" + swarmID + "/actors"
	r, b, _ = ts.do(alice, "POST", path, ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "form-actor"}, "actorTemplate": map[string]string{"name": "claude-proxmox-form-runtime"}})
	expectStatus(t, "create form actor", r, b, 201)
	actorUID := b["metadata"].(map[string]any)["uid"].(string)
	for range 4 {
		ts.srv.managed.reconcileOnce(context.Background())
	}
	a, err := reopened.GetManagedActor(context.Background(), actorUID)
	if err != nil || a.Phase != "running" || fake.creates != 1 || fake.starts != 1 {
		t.Fatalf("form actor lifecycle: phase=%s creates=%d starts=%d err=%v", a.Phase, fake.creates, fake.starts, err)
	}
	r, b, _ = ts.do(alice, "POST", path+"/form-actor", ts.csrf(alice), map[string]string{"action": "stop"})
	expectStatus(t, "stop form actor", r, b, 200)
	for range 2 {
		ts.srv.managed.reconcileOnce(context.Background())
	}
	a, err = reopened.GetManagedActor(context.Background(), actorUID)
	if err != nil || a.Phase != "stopped" {
		t.Fatalf("form actor stop: phase=%s err=%v", a.Phase, err)
	}
	r, b, _ = ts.do(admin, "DELETE", "/api/proxmox-pools/form-runtime", ts.csrf(admin), nil)
	expectStatus(t, "allocated pool deletion", r, b, 409)
	r, b, _ = ts.do(alice, "DELETE", path+"/form-actor", ts.csrf(alice), nil)
	expectStatus(t, "delete form actor", r, b, 202)
	for range 3 {
		ts.srv.managed.reconcileOnce(context.Background())
	}
	r, b, _ = ts.do(admin, "DELETE", "/api/proxmox-pools/form-runtime", ts.csrf(admin), nil)
	expectStatus(t, "delete empty pool", r, b, 204)
	r, b, _ = ts.do(admin, "DELETE", "/api/proxmox-connections/"+uid, ts.csrf(admin), nil)
	expectStatus(t, "delete unused connection", r, b, 204)
}

func TestProxmoxConnectionStrictPayload(t *testing.T) {
	ts, _, _, _, _, _ := managedSetup(t)
	admin := newClient(t)
	ts.login(admin, "root", "root-password-1")
	_, ca := selfSignFake(t, "strict-form")
	body := connectionBody("strict-form", string(ca))
	raw, _ := json.Marshal(body)
	for _, payload := range []string{string(raw) + ` {}`, strings.TrimSuffix(string(raw), "}") + `,"name":"second"}`, strings.TrimSuffix(string(raw), "}") + `,"extra":"secret"}`} {
		req, _ := http.NewRequest("POST", ts.http.URL+"/api/proxmox-connections", strings.NewReader(payload))
		req.Header.Set("Origin", ts.http.URL)
		req.Header.Set("X-CSRFToken", ts.csrf(admin))
		resp, err := admin.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatalf("unsafe payload accepted: %d", resp.StatusCode)
		}
	}
	body["name"] = "brood-proxmox"
	r, b, _ := ts.do(admin, "POST", "/api/proxmox-connections", ts.csrf(admin), body)
	expectStatus(t, "env name collision", r, b, 409)
	body["name"] = "bad-runtime"
	body["template"] = "local:vztmpl/brood-claude-proxmox-a.tar"
	r, b, _ = ts.do(admin, "POST", "/api/proxmox-connections", ts.csrf(admin), body)
	expectStatus(t, "partial runtime", r, b, 422)

	// Коллизия после перезапуска также закрыта, даже если БД создана отдельно.
	if _, err := ts.srv.store.CreateManagedConnection(context.Background(), store.ManagedConnection{Name: "brood-proxmox", TokenSecret: "collision-secret"}); err != nil {
		t.Fatal(err)
	}
	m := &managedProvider{server: ts.srv, registry: ts.srv.managed.registry}
	if err := m.initialize(Config{}); err == nil || strings.Contains(err.Error(), "collision-secret") {
		t.Fatal("startup collision was accepted or leaked credentials")
	}
}

func TestProxmoxConnectionRuntimeValidation(t *testing.T) {
	ts, _, _, _, _, _ := managedSetup(t)
	admin := newClient(t)
	ts.login(admin, "root", "root-password-1")
	_, ca := selfSignFake(t, "runtime-validation")
	for _, tc := range []struct {
		name  string
		field string
		value any
	}{
		{"template", "template", "local:vztmpl/other.tar"},
		{"digest", "templateDigest", "sha256:bad"},
		{"null", "capacity", nil},
		{"zero", "capacity", json.RawMessage(`{"actors":0,"cpuMillicores":"2000","memoryBytes":"2147483648","diskGiB":8}`)},
		{"numeric-cpu", "capacity", json.RawMessage(`{"actors":2,"cpuMillicores":2000,"memoryBytes":"2147483648","diskGiB":8}`)},
		{"fraction", "capacity", json.RawMessage(`{"actors":2.5,"cpuMillicores":"2000","memoryBytes":"2147483648","diskGiB":8}`)},
		{"small", "capacity", json.RawMessage(`{"actors":2,"cpuMillicores":"1999","memoryBytes":"2147483648","diskGiB":8}`)},
		{"overflow", "capacity", json.RawMessage(`{"actors":2,"cpuMillicores":"2000","memoryBytes":"2147483648","diskGiB":9223372036854775807}`)},
		{"duplicate", "capacity", json.RawMessage(`{"actors":2,"actors":3,"cpuMillicores":"2000","memoryBytes":"2147483648","diskGiB":8}`)},
		{"unknown", "capacity", json.RawMessage(`{"actors":2,"cpuMillicores":"2000","memoryBytes":"2147483648","diskGiB":8,"extra":1}`)},
		{"long-name", "name", strings.Repeat("x", 49)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := connectionBody("invalid-runtime", string(ca))
			body["template"] = "local:vztmpl/brood-claude-proxmox-aaaaaaaa.tar.zst"
			body["templateDigest"] = "sha256:" + strings.Repeat("a", 64)
			body["capacity"] = map[string]any{"actors": 2, "cpuMillicores": "2000", "memoryBytes": "2147483648", "diskGiB": 8}
			body[tc.field] = tc.value
			r, b, raw := ts.do(admin, "POST", "/api/proxmox-connections", ts.csrf(admin), body)
			expectStatus(t, "runtime validation", r, b, 422)
			if strings.Contains(raw, "form-secret-value") || strings.Contains(raw, "BEGIN CERTIFICATE") {
				t.Fatal("validation leaked credentials")
			}
		})
	}
}

func connectionBody(name, caPEM string) map[string]any {
	return map[string]any{"name": name, "endpoint": "https://pve2.invalid:8006",
		"tokenID": "form@pve!brood", "tokenSecret": "form-secret-value",
		"caPEM": caPEM, "node": "n2", "resourcePool": "brood",
		"storage": "local", "network": "vmbr0"}
}

func TestProxmoxConnectionAdminAPI(t *testing.T) {
	ts, _, _, _, _, _ := managedSetup(t)
	_, caPEM := selfSignFake(t, "pve-test-ca")
	admin := newClient(t)
	ts.login(admin, "root", "root-password-1")
	r, b, _ := ts.do(admin, "POST", "/api/proxmox-connections", ts.csrf(admin), connectionBody("pve-form", string(caPEM)))
	expectStatus(t, "create connection", r, b, http.StatusCreated)
	if b["tokenSecret"] != nil {
		t.Fatalf("response leaked the secret: %#v", b)
	}
	if b["credentialConfigured"] != true {
		t.Fatalf("connection DTO: %#v", b)
	}
	uid, _ := b["uid"].(string)
	if uid == "" {
		t.Fatal("missing uid")
	}
	// Список — без секрета.
	r, b, _ = ts.do(admin, "GET", "/api/proxmox-connections", "", nil)
	expectStatus(t, "list", r, b, http.StatusOK)
	items, _ := b["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["tokenSecret"] != nil {
		t.Fatalf("items leaked or lost: %#v", items)
	}
	// Дубликат имени — конфликт.
	r, b, _ = ts.do(admin, "POST", "/api/proxmox-connections", ts.csrf(admin), connectionBody("pve-form", string(caPEM)))
	expectStatus(t, "duplicate name", r, b, http.StatusConflict)
	// Кривой endpoint — четвёрка с причиной.
	bad := connectionBody("pve-bad", string(caPEM))
	bad["endpoint"] = "http://plain.invalid"
	r, b, _ = ts.do(admin, "POST", "/api/proxmox-connections", ts.csrf(admin), bad)
	expectStatus(t, "bad endpoint", r, b, http.StatusUnprocessableEntity)
	// Удаление.
	r, b, _ = ts.do(admin, "DELETE", "/api/proxmox-connections/"+uid, ts.csrf(admin), nil)
	expectStatus(t, "delete", r, b, http.StatusNoContent)
	list, err := ts.srv.store.ListManagedConnections(nil2ctx())
	if err != nil || len(list) != 0 {
		t.Fatalf("delete left rows: %+v %v", list, err)
	}
	// Не-админ не создаёт: bob из managedSetup — участник проекта, не админ.
	bob := bobFrom(t, ts)
	r, b, _ = ts.do(bob, "POST", "/api/proxmox-connections", ts.csrf(bob), connectionBody("pve-form2", string(caPEM)))
	expectStatus(t, "non-admin create", r, b, http.StatusForbidden)
}

// bobFrom — не-админ: отдельный вход для существующего пользователя bob.
func bobFrom(t *testing.T, ts *substrateServer) *http.Client {
	t.Helper()
	client := newClient(t)
	ts.login(client, "bob", "bob-password-1")
	return client
}

func TestProxmoxConnectionAppearsInPoolInventory(t *testing.T) {
	ts, _, _, _, _, _ := managedSetup(t)
	_, caPEM := selfSignFake(t, "pve-inv-ca")
	admin := newClient(t)
	ts.login(admin, "root", "root-password-1")
	r, b, _ := ts.do(admin, "POST", "/api/proxmox-connections", ts.csrf(admin), map[string]any{
		"name": "pve-inv", "endpoint": "https://pve3.invalid:8006", "tokenID": "inv@pve!brood",
		"tokenSecret": "inv-secret", "caPEM": string(caPEM), "node": "n3",
		"resourcePool": "brood", "storage": "local", "network": "vmbr0"})
	expectStatus(t, "create", r, b, http.StatusCreated)
	// Инвентарь пулов показывает новое подключение рядом с env-настроенным.
	r, b, _ = ts.do(admin, "GET", "/api/proxmox-pools", "", nil)
	expectStatus(t, "inventory", r, b, http.StatusOK)
	items, _ := b["items"].([]any)
	var found bool
	for _, item := range items {
		dto := item.(map[string]any)
		if dto["name"] == "pve-inv" {
			found = true
			if dto["endpoint"] != "https://pve3.invalid:8006" || dto["credentialConfigured"] != true {
				t.Fatalf("connection pool DTO: %#v", dto)
			}
		}
	}
	if !found {
		t.Fatalf("created connection missing from pool inventory: %#v", items)
	}
}
