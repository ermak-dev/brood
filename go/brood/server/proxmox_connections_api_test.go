package server

// FEATURE: Админский API подключений Proxmox (#118).
// SPEC: POST/GET/DELETE /api/proxmox-connections: валидация как у env-конфигурации,
// секрет write-only (не возвращается ни при создании, ни в списке), не-админ
// и аноним получают отказ; подключение попадает в список пулов как read-only
// до активации.
import (
	"net/http"
	"testing"
)

var testCAPEM string

func connectionBody(name string) map[string]any {
	return map[string]any{"name": name, "endpoint": "https://pve2.invalid:8006",
		"tokenID": "form@pve!brood", "tokenSecret": "form-secret-value",
		"caPEM": testCAPEM, "node": "n2", "resourcePool": "brood",
		"storage": "local", "network": "vmbr0"}
}

func TestProxmoxConnectionAdminAPI(t *testing.T) {
	ts, _, _, _, _, _ := managedSetup(t)
	_, caPEM := selfSignFake(t, "pve-test-ca")
	testCAPEM = string(caPEM)
	admin := newClient(t)
	ts.login(admin, "root", "root-password-1")
	r, b, _ := ts.do(admin, "POST", "/api/proxmox-connections", ts.csrf(admin), connectionBody("pve-form"))
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
	r, b, _ = ts.do(admin, "POST", "/api/proxmox-connections", ts.csrf(admin), connectionBody("pve-form"))
	expectStatus(t, "duplicate name", r, b, http.StatusConflict)
	// Кривой endpoint — четвёрка с причиной.
	bad := connectionBody("pve-bad")
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
	r, b, _ = ts.do(bob, "POST", "/api/proxmox-connections", ts.csrf(bob), connectionBody("pve-form2"))
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
