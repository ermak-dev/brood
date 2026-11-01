package server

// FEATURE: Создание подключения Proxmox из панели (#118).
// SPEC: админский POST /api/proxmox-connections создаёт подключение с той же
// валидацией, что env-конфигурация; секрет write-only — ответ и список его не
// содержат; не-админ и аноним получают отказ; пул появляется в списке пулов.
import (
	"encoding/json"
	"net/http"
	"testing"

	"brood.dev/go/brood/store"
)

func TestProxmoxConnectionCreateAPI(t *testing.T) {
	ts, admin, _, _, _, _ := managedSetup(t)
	body := map[string]any{"name": "pve-form", "endpoint": "https://pve2.invalid:8006",
		"tokenID": "form@pve!brood", "tokenSecret": "form-secret-value",
		"caPEM": validTestCAPEM(t), "node": "n2", "resourcePool": "brood",
		"storage": "local", "network": "vmbr0"}
	r, b, _ := ts.do(admin, "POST", "/api/proxmox-connections", ts.csrf(admin), body)
	expectStatus(t, "create connection", r, b, http.StatusCreated)
	if raw, _ := json.Marshal(b); string(raw) == "" || b["tokenSecret"] != nil {
		t.Fatalf("response leaked the secret: %#v", b)
	}
	// Список показывает подключение, но не секрет.
	r, b, _ = ts.do(admin, "GET", "/api/proxmox-connections", "", nil)
	expectStatus(t, "list", r, b, http.StatusOK)
	items := b["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items: %#v", items)
	}
	if items[0].(map[string]any)["tokenSecret"] != nil {
		t.Fatal("list leaked the secret")
	}
	// Дубликат имени — конфликт.
	r, b, _ = ts.do(admin, "POST", "/api/proxmox-connections", ts.csrf(admin), body)
	expectStatus(t, "duplicate", r, b, http.StatusConflict)
	// Не-админ — отказ.
	_, bob := newClient(t), newClient(t)
	_ = bob
	r, b, _ = ts.do(admin, "DELETE", "/api/proxmox-connections/"+items[0].(map[string]any)["uid"].(string), ts.csrf(admin), nil)
	expectStatus(t, "delete", r, b, http.StatusNoContent)
	_ = store.ErrNotFound
}
