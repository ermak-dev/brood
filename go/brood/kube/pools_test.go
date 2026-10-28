package kube

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	image       = "localhost:5002/worker@sha256:" + strings.Repeat("a", 64)
	microvmImg  = "localhost:5002/ateworker-microvm@sha256:" + strings.Repeat("b", 64)
	legacyImage = "localhost:5002/worker@sha256:" + strings.Repeat("c", 64)
	images      = map[string]string{"gvisor": image, "microvm": microvmImg}
)

// pool — типичное тело WorkerPool, как в проверках панели.
func pool() map[string]any {
	return map[string]any{
		"apiVersion": "ate.dev/v1", "kind": "WorkerPool",
		"metadata": map[string]any{"name": "agents", "namespace": "brood", "uid": "u1",
			"resourceVersion": "17", "creationTimestamp": "2026-10-25T00:00:00Z"},
		"spec": map[string]any{"replicas": 1, "workerImage": image,
			"sandboxClasses": []any{map[string]any{"name": "gvisor"}},
			"template": map[string]any{"resources": map[string]any{
				"requests": map[string]any{"cpu": "250m", "memory": "2Gi"},
				"limits":   map[string]any{"cpu": "2", "memory": "2Gi"}}}},
		"status": map[string]any{"replicas": 1, "readyReplicas": 1},
	}
}

// body — JSON-байты объекта.
func body(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

// asKube вынимает *KubeError из отказа проверки.
func asKube(t *testing.T, err error) *KubeError {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	kubeErr, ok := err.(*KubeError)
	if !ok {
		t.Fatalf("expected *KubeError, got %T: %v", err, err)
	}
	return kubeErr
}

// writeFile кладёт файл во временный каталог проверки.
func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// fakeAPI — подделка Kubernetes: записи запросов и страниц списка.
type fakeAPI struct {
	mu        sync.Mutex
	handler   http.HandlerFunc
	auths     []string // заголовки authorization по запросам
	lists     []string // значения continue по запросам списка
	sent      []string // тела изменяющих запросов
	server    *httptest.Server
	caFile    string
	tokenFile string
}

// startAPI поднимает HTTPS-подделку Kubernetes и пишет токен и её корень.
func startAPI(t *testing.T, handler http.HandlerFunc) *fakeAPI {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	cert := server.Certificate()
	pemData := "-----BEGIN CERTIFICATE-----\n" +
		base64Lines(cert.Raw) +
		"-----END CERTIFICATE-----\n"
	api := &fakeAPI{handler: handler, server: server,
		caFile:    writeFile(t, "ca.pem", pemData),
		tokenFile: writeFile(t, "token", "first\n")}
	server.Config.Handler = http.HandlerFunc(api.route)
	return api
}

// route оборачивает обработчик подделки записью авторизации.
func (f *fakeAPI) route(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.auths = append(f.auths, r.Header.Get("Authorization"))
	switch {
	case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "limit"):
		f.lists = append(f.lists, r.URL.Query().Get("continue"))
	case r.Method != http.MethodGet:
		raw, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		f.sent = append(f.sent, string(raw))
	}
	f.mu.Unlock()
	f.handler(w, r)
}

// base64Lines кодирует DER в PEM-строки.
func base64Lines(der []byte) string {
	encoded := base64.StdEncoding.EncodeToString(der)
	var out strings.Builder
	for i := 0; i < len(encoded); i += 64 {
		end := i + 64
		if end > len(encoded) {
			end = len(encoded)
		}
		out.WriteString(encoded[i:end] + "\n")
	}
	return out.String()
}

// pools — сервис пула над подделкой.
func (f *fakeAPI) pools() *Pools {
	p, err := NewPools(f.server.URL, f.tokenFile, f.caFile, mustJSON(images), "")
	if err != nil {
		panic(err)
	}
	return p
}

func mustJSON(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}

// ─── endpoint и данные доступа ─────────────────────────────────────────────

func TestBadEndpointIsARefusal(t *testing.T) {
	for _, endpoint := range []string{"http://api", "https://user:password@api",
		"https://api/path", "https://api:invalid", "https://api:99999", "https://[broken"} {
		_, err := NewClient(endpoint, "/token", "/ca")
		kubeErr := asKube(t, err)
		if kubeErr.Status != 503 || !strings.Contains(kubeErr.Detail, "KUBE_API_URL") {
			t.Errorf("%s: got %+v, want 503 naming KUBE_API_URL", endpoint, kubeErr)
		}
	}
}

func TestNewPoolsRequiresAllSettings(t *testing.T) {
	api := startAPI(t, func(http.ResponseWriter, *http.Request) {})
	_, err := NewPools("", api.tokenFile, api.caFile, mustJSON(images), "")
	kubeErr := asKube(t, err)
	if kubeErr.Status != 503 || !strings.Contains(kubeErr.Detail, "KUBE_API_URL") {
		t.Fatalf("got %+v, want 503 naming the missing settings", kubeErr)
	}
}

func TestMissingTokenFileIsAServiceRefusal(t *testing.T) {
	api := startAPI(t, func(http.ResponseWriter, *http.Request) {})
	service, err := NewPools(api.server.URL, filepath.Join(t.TempDir(), "missing"),
		api.caFile, mustJSON(images), "")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	_, err = service.Get(context.Background(), "agents")
	kubeErr := asKube(t, err)
	if kubeErr.Status != 503 || !strings.Contains(kubeErr.Detail, "KUBE_API_TOKEN") {
		t.Fatalf("got %+v, want 503 naming KUBE_API_TOKEN", kubeErr)
	}
}

// ─── транспорт ─────────────────────────────────────────────────────────────

func TestListPaginatesAndRotatesToken(t *testing.T) {
	api := startAPI(t, func(w http.ResponseWriter, r *http.Request) {
		// Первая страница — два пула и закодированный токен продолжения.
		if r.Method == http.MethodGet && r.URL.Query().Get("continue") == "" {
			w.Write(body(t, map[string]any{"items": []any{pool(), pool()},
				"metadata": map[string]any{"continue": "a/b+"}}))
			return
		}
		w.Write(body(t, map[string]any{"items": []any{}, "metadata": map[string]any{}}))
	})
	service := api.pools()
	ctx := context.Background()
	out, err := service.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d items, want 2", len(out))
	}
	if err := os.WriteFile(api.tokenFile, []byte("second\n"), 0o600); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if _, err = service.Get(ctx, "agents"); err != nil {
		t.Fatalf("get: %v", err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.lists) != 2 || api.lists[0] != "" || api.lists[1] != "a/b+" {
		t.Fatalf("continue tokens walked wrong: %v", api.lists)
	}
	want := []string{"Bearer first", "Bearer second"}
	if len(api.auths) != 3 || api.auths[0] != want[0] || api.auths[2] != want[1] {
		t.Fatalf("authorization headers %v, want %v on 1st and 3rd", api.auths, want)
	}
}

func TestErrorStatusesKeepKubernetesReason(t *testing.T) {
	api := startAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(409)
		w.Write(body(t, map[string]any{"message": "object has been modified"}))
	})
	service := api.pools()
	_, err := service.Update(context.Background(), "agents", body(t, pool()))
	kubeErr := asKube(t, err)
	if kubeErr.Status != 409 || !strings.Contains(kubeErr.Detail, "modified") {
		t.Fatalf("got %+v, want 409 with the Kubernetes message", kubeErr)
	}
}

func TestNonJSONAndRedirectsAreGatewayFailures(t *testing.T) {
	for _, test := range []struct {
		status  int
		payload string
	}{
		{500, "<html>broken</html>"},
		{302, ""},
	} {
		api := startAPI(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(test.status)
			w.Write([]byte(test.payload))
		})
		_, err := api.pools().Get(context.Background(), "agents")
		kubeErr := asKube(t, err)
		if kubeErr.Status != 502 {
			t.Errorf("%d: got %+v, want 502", test.status, kubeErr)
		}
		if strings.Contains(kubeErr.Detail, "first") {
			t.Errorf("%d: detail leaks credentials: %s", test.status, kubeErr.Detail)
		}
	}
}

func TestTimeoutIsAServiceRefusal(t *testing.T) {
	block := make(chan struct{})
	api := startAPI(t, func(w http.ResponseWriter, r *http.Request) {
		<-block
	})
	t.Cleanup(func() { close(block) })
	service := api.pools()
	service.client.timeout = 50 * time.Millisecond
	_, err := service.Get(context.Background(), "agents")
	kubeErr := asKube(t, err)
	if kubeErr.Status != 503 {
		t.Fatalf("got %+v, want 503", kubeErr)
	}
}

func TestInvalidListShapeIsRefused(t *testing.T) {
	api := startAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write(body(t, map[string]any{"items": "not a list"}))
	})
	_, err := api.pools().List(context.Background())
	kubeErr := asKube(t, err)
	if kubeErr.Status != 502 {
		t.Fatalf("got %+v, want 502", kubeErr)
	}
}

// ─── образы воркеров из настроек ───────────────────────────────────────────

func TestParseImagesFailClosed(t *testing.T) {
	for _, test := range []struct{ raw, single string }{
		{"not json", ""},
		{"[]", ""},
		{"{}", ""},
		{`{"gvisor":"worker:latest"}`, ""},
		{`{"gvisor":"` + image + `","fullvm":"` + microvmImg + `"}`, ""},
		{"", ""},
		{"", "worker:latest"},
	} {
		_, err := ParseImages(test.raw, test.single)
		kubeErr := asKube(t, err)
		if kubeErr.Status != 503 {
			t.Errorf("%q/%q: got %+v, want 503", test.raw, test.single, kubeErr)
		}
		if !strings.Contains(kubeErr.Detail, "WORKER_POOL_IM") {
			t.Errorf("%q/%q: detail %q must name the setting", test.raw, test.single, kubeErr.Detail)
		}
	}
	parsed, err := ParseImages(mustJSON(images), "")
	if err != nil || parsed["microvm"] != microvmImg {
		t.Fatalf("parse: %v %v", parsed, err)
	}
	parsed, err = ParseImages("", image)
	if err != nil || len(parsed) != 1 || parsed["gvisor"] != image {
		t.Fatalf("legacy parse: %v %v", parsed, err)
	}
}

func TestDefaultsExposeInstalledImages(t *testing.T) {
	api := startAPI(t, func(http.ResponseWriter, *http.Request) {})
	service := api.pools()
	defaults := service.Defaults()
	if defaults["workerImage"] != image || fmt.Sprint(defaults["workerImages"]) != fmt.Sprint(images) {
		t.Fatalf("defaults %v", defaults)
	}
}

// ─── создание: идентичность и допустимые поля ─────────────────────────────

func TestCreateForcesIdentityAndDropsServerFields(t *testing.T) {
	var created string
	api := startAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			raw := make([]byte, r.ContentLength)
			r.Body.Read(raw)
			created = string(raw)
			w.Write(raw)
			return
		}
		w.Write(body(t, pool()))
	})
	in := pool()
	in["metadata"].(map[string]any)["namespace"] = "other"
	saved, err := api.pools().Create(context.Background(), body(t, in))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if saved["kind"] != "WorkerPool" {
		t.Fatalf("saved %v", saved)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(created), &sent); err != nil {
		t.Fatalf("sent body: %v", err)
	}
	metadata := sent["metadata"].(map[string]any)
	if len(metadata) != 2 || metadata["name"] != "agents" || metadata["namespace"] != Namespace {
		t.Fatalf("sent metadata %v, want name and namespace only", metadata)
	}
	if _, ok := sent["status"]; ok {
		t.Fatal("status must not be sent")
	}
	if fmt.Sprint(sent["spec"]) != fmt.Sprint(in["spec"]) {
		t.Fatalf("sent spec %v", sent["spec"])
	}
}

func TestDangerousAndUnknownFieldsAreRefused(t *testing.T) {
	mutators := []func(map[string]any){
		func(b map[string]any) {
			b["spec"].(map[string]any)["workerImage"] = "evil@sha256:" + strings.Repeat("b", 64)
		},
		func(b map[string]any) {
			b["spec"].(map[string]any)["template"].(map[string]any)["serviceAccountName"] = "brood"
		},
		func(b map[string]any) {
			b["spec"].(map[string]any)["template"].(map[string]any)["annotations"] = map[string]any{"evil": "value"}
		},
		func(b map[string]any) {
			b["spec"].(map[string]any)["sandboxClasses"] = []any{map[string]any{"name": "microvm"}}
		},
		func(b map[string]any) { b["spec"].(map[string]any)["replicas"] = true },
		func(b map[string]any) { b["spec"].(map[string]any)["replicas"] = -1 },
		func(b map[string]any) { b["spec"].(map[string]any)["replicas"] = 1.5 },
		func(b map[string]any) { b["spec"].(map[string]any)["replicas"] = 2147483648 },
		func(b map[string]any) { b["spec"].(map[string]any)["unrecognized"] = "value" },
		func(b map[string]any) { b["spec"].(map[string]any)["template"] = []any{} },
		func(b map[string]any) {
			r := b["spec"].(map[string]any)["template"].(map[string]any)["resources"].(map[string]any)
			r["limits"].(map[string]any)["example.com/gpu"] = "1"
		},
		func(b map[string]any) {
			r := b["spec"].(map[string]any)["template"].(map[string]any)["resources"].(map[string]any)
			r["limits"].(map[string]any)["memory"] = map[string]any{}
		},
		func(b map[string]any) {
			b["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]any{"uid": "server"}}
		},
		func(b map[string]any) { b["metadata"].(map[string]any)["finalizers"] = []any{"evil.example/block"} },
		func(b map[string]any) {
			b["spec"].(map[string]any)["sandboxClasses"] = []any{map[string]any{"name": "gvisor", "extra": 1}}
		},
		func(b map[string]any) { b["metadata"].(map[string]any)["labels"] = map[string]any{"count": 3} },
	}
	for _, mutate := range mutators {
		in := pool()
		mutate(in)
		api := startAPI(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Write(body(t, pool()))
		})
		_, err := api.pools().Create(context.Background(), body(t, in))
		if asKube(t, err).Status != 400 {
			t.Errorf("mutation accepted: %v", err)
		}
		api.mu.Lock()
		sent := len(api.sent)
		api.mu.Unlock()
		if sent != 0 {
			t.Fatal("invalid body reached Kubernetes")
		}
	}
}

func TestClassAndImageMustMatch(t *testing.T) {
	for _, test := range []struct{ class, image string }{
		{"microvm", image},
		{"gvisor", microvmImg},
		{"lxc", microvmImg},
		{"microvm", "evil@sha256:" + strings.Repeat("c", 64)},
	} {
		in := pool()
		spec := in["spec"].(map[string]any)
		spec["workerImage"] = test.image
		spec["sandboxClasses"] = []any{map[string]any{"name": test.class}}
		api := startAPI(t, func(http.ResponseWriter, *http.Request) {})
		_, err := api.pools().Create(context.Background(), body(t, in))
		if asKube(t, err).Status != 400 {
			t.Errorf("class %s image %s: accepted", test.class, test.image)
		}
	}
}

func TestMicrovmCreateKeepsConfigRef(t *testing.T) {
	var created string
	api := startAPI(t, func(w http.ResponseWriter, r *http.Request) {
		raw := make([]byte, r.ContentLength)
		r.Body.Read(raw)
		created = string(raw)
		w.Write(raw)
	})
	in := pool()
	spec := in["spec"].(map[string]any)
	spec["workerImage"] = microvmImg
	spec["sandboxClasses"] = []any{map[string]any{"name": "microvm", "configRef": map[string]any{"name": "microvm"}}}
	if _, err := api.pools().Create(context.Background(), body(t, in)); err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.Contains(created, `"configRef":{"name":"microvm"}`) {
		t.Fatalf("configRef lost: %s", created)
	}
}

func TestNonObjectBodiesAreRefused(t *testing.T) {
	api := startAPI(t, func(http.ResponseWriter, *http.Request) {})
	for _, raw := range []string{`[]`, `null`, `not json`, `{"spec": {}}`} {
		_, err := api.pools().Create(context.Background(), []byte(raw))
		if asKube(t, err).Status != 400 {
			t.Errorf("%q accepted", raw)
		}
	}
}

// ─── изменение: предусловия и неизменность ────────────────────────────────

func TestUpdatePassesPreconditionsAndKeepsUrlName(t *testing.T) {
	var updated string
	api := startAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			raw := make([]byte, r.ContentLength)
			r.Body.Read(raw)
			updated = string(raw)
			w.Write(raw)
			return
		}
		w.Write(body(t, pool()))
	})
	in := pool()
	in["spec"].(map[string]any)["replicas"] = 0
	in["metadata"].(map[string]any)["name"] = "other"
	if _, err := api.pools().Update(context.Background(), "agents", body(t, in)); err != nil {
		t.Fatalf("update: %v", err)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(updated), &sent); err != nil {
		t.Fatalf("sent body: %v", err)
	}
	metadata := sent["metadata"].(map[string]any)
	if metadata["name"] != "agents" || metadata["namespace"] != Namespace ||
		metadata["uid"] != "u1" || metadata["resourceVersion"] != "17" {
		t.Fatalf("sent metadata %v", metadata)
	}
	if sent["spec"].(map[string]any)["replicas"] != float64(0) {
		t.Fatalf("sent spec %v", sent["spec"])
	}
}

func TestUpdateRefusesMissingPreconditions(t *testing.T) {
	api := startAPI(t, func(http.ResponseWriter, *http.Request) {})
	for _, raw := range []string{`{"spec": {}}`, `[]`, `null`, `{"metadata": {"uid": ""}}`,
		`{"metadata": {"uid": "u1", "resourceVersion": " "}}`} {
		_, err := api.pools().Update(context.Background(), "agents", []byte(raw))
		if asKube(t, err).Status != 400 {
			t.Errorf("%q accepted", raw)
		}
		if err := api.pools().Delete(context.Background(), "agents", []byte(raw)); err == nil {
			t.Errorf("delete %q accepted", raw)
		}
	}
}

func TestUpdateConflictOnStaleVersion(t *testing.T) {
	current := pool()
	current["metadata"].(map[string]any)["resourceVersion"] = "18"
	api := startAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write(body(t, current))
	})
	_, err := api.pools().Update(context.Background(), "agents", body(t, pool()))
	kubeErr := asKube(t, err)
	if kubeErr.Status != 409 || !strings.Contains(kubeErr.Detail, "modified") {
		t.Fatalf("got %+v, want 409 with modified reason", kubeErr)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.sent) != 0 {
		t.Fatal("conflicting update reached Kubernetes")
	}
}

func TestSandboxClassIsImmutable(t *testing.T) {
	api := startAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write(body(t, pool()))
	})
	in := pool()
	spec := in["spec"].(map[string]any)
	spec["workerImage"] = microvmImg
	spec["sandboxClasses"] = []any{map[string]any{"name": "microvm"}}
	_, err := api.pools().Update(context.Background(), "agents", body(t, in))
	kubeErr := asKube(t, err)
	if kubeErr.Status != 400 || !strings.Contains(kubeErr.Detail, "immutable") {
		t.Fatalf("got %+v, want 400 with immutable reason", kubeErr)
	}
}

func TestExistingPoolKeepsOlderDigestAfterRotation(t *testing.T) {
	// Текущий пул всё ещё катит прежний digest: обновление оставляет его,
	// создание нового пула с ним уже запрещено.
	current := pool()
	current["spec"].(map[string]any)["workerImage"] = legacyImage
	api := startAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Write(body(t, current))
			return
		}
		w.Write(body(t, current))
	})
	in := pool()
	in["spec"].(map[string]any)["workerImage"] = legacyImage
	if _, err := api.pools().Update(context.Background(), "agents", body(t, in)); err != nil {
		t.Fatalf("update with old digest: %v", err)
	}
	api.mu.Lock()
	sent := api.sent
	api.mu.Unlock()
	if len(sent) != 1 || !strings.Contains(sent[0], legacyImage[:40]) {
		t.Fatalf("update did not keep the old digest: %v", sent)
	}
	api.mu.Lock()
	api.sent = nil
	api.mu.Unlock()
	_, err := api.pools().Create(context.Background(), body(t, in))
	if asKube(t, err).Status != 400 {
		t.Fatal("create accepted a stale digest")
	}
}

// ─── удаление ─────────────────────────────────────────────────────────────

func TestDeletePassesForegroundPreconditions(t *testing.T) {
	var deleted string
	api := startAPI(t, func(w http.ResponseWriter, r *http.Request) {
		raw := make([]byte, r.ContentLength)
		r.Body.Read(raw)
		deleted = string(raw)
		w.Write(body(t, pool()))
	})
	if err := api.pools().Delete(context.Background(), "agents",
		body(t, map[string]any{"metadata": map[string]any{"uid": "u1", "resourceVersion": "17"}})); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(deleted), &sent); err != nil {
		t.Fatalf("sent body: %v", err)
	}
	if fmt.Sprint(sent["preconditions"]) != fmt.Sprint(map[string]any{"uid": "u1", "resourceVersion": "17"}) {
		t.Fatalf("preconditions %v", sent["preconditions"])
	}
	if sent["propagationPolicy"] != "Foreground" {
		t.Fatalf("propagationPolicy %v", sent["propagationPolicy"])
	}
}

// ─── имена ────────────────────────────────────────────────────────────────

func TestNamesMustBeDNSSubdomains(t *testing.T) {
	api := startAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write(body(t, pool()))
	})
	service := api.pools()
	long := strings.Repeat("a", 63) + ".agents"
	in := pool()
	in["metadata"].(map[string]any)["name"] = long
	if _, err := service.Create(context.Background(), body(t, in)); err != nil {
		t.Fatalf("long valid name refused: %v", err)
	}
	for _, name := range []string{"..", ".agents", "agents.", "Agents", strings.Repeat("a", 64)} {
		in := pool()
		in["metadata"].(map[string]any)["name"] = name
		_, err := service.Create(context.Background(), body(t, in))
		if asKube(t, err).Status != 400 {
			t.Errorf("name %q accepted", name)
		}
		if _, err = service.Get(context.Background(), name); asKube(t, err).Status != 400 {
			t.Errorf("lookup name %q accepted", name)
		}
	}
}
