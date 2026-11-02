package server

// FEATURE: Administration calls use existing provider clients and a private model client.
// SPEC: Drain never evacuates; native cannot undrain; model metadata is allowlisted.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"brood.dev/go/brood/store"
	"brood.dev/go/brood/substrate"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// adminNativeControl models the drain state transition missing from the general read-only fake.
type adminNativeControl struct{ *fakeAteapi }

func (f *adminNativeControl) DrainWorker(ctx context.Context, req *ateapi.DrainWorkerRequest) (*ateapi.DrainWorkerResponse, error) {
	response, err := f.fakeAteapi.DrainWorker(ctx, req)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	response.Worker.Status.State = ateapi.WorkerState_WORKER_STATE_DRAINING
	return response, nil
}
func TestAdminCommandNativeDrainAndInventory(t *testing.T) {
	fake := newFakeAteapi()
	fake.workers["worker-1"] = &ateapi.Worker{Metadata: fakeMeta("worker-1", ""), WorkerPool: "pool", Status: &ateapi.WorkerStatus{State: ateapi.WorkerState_WORKER_STATE_ACTIVE}}
	srv := adminCommandServer(t)
	cert, caPEM := selfSignFake(t, substrate.Audience)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rpc := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}})))
	ateapi.RegisterControlServer(rpc, &adminNativeControl{fake})
	go rpc.Serve(listener)
	t.Cleanup(rpc.Stop)
	ca := writeFileFake(t, "admin-ca", "")
	if err := os.WriteFile(ca, caPEM, 0600); err != nil {
		t.Fatal(err)
	}
	srv.ateapi, err = substrate.New(writeFileFake(t, "admin-token", "test-token"), ca, listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	result, err := runAdminCommand(srv, "admin", "", "node", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result.Data), `"name":"native:worker-1"`) {
		t.Fatalf("worker missing: %s", result.Data)
	}
	result, err = runAdminCommand(srv, "admin", "", "node_drain", `{"name":"native:worker-1"}`)
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	state := fake.workers["worker-1"].Status.State
	fake.mu.Unlock()
	if state != ateapi.WorkerState_WORKER_STATE_DRAINING {
		t.Fatalf("native worker not drained: %v", state)
	}
	if !strings.Contains(string(result.Data), `"evacuation_confirmed":false`) {
		t.Fatalf("untruthful drain: %s", result.Data)
	}
	if _, err := runAdminCommand(srv, "admin", "", "node_drain", `{"name":"managed:worker-1"}`); err == nil {
		t.Fatal("foreign provider resolved native worker")
	}
}

func TestAdminCommandManagedDrainEnable(t *testing.T) {
	ts, _, _, swarmID, pve, _ := managedSetup(t)
	root, err := ts.srv.store.GetUserByUsername(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	p, err := ts.srv.managed.poolByName(context.Background(), "brood-proxmox")
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct{ op, state string }{{"node_drain", "draining"}, {"node_up", "enabled"}} {
		if _, err := runAdminCommand(ts.srv, root.ID, "", step.op, `{"name":"managed:brood-proxmox"}`); err != nil {
			t.Fatal(err)
		}
		got, err := ts.srv.store.GetManagedPool(context.Background(), p.UID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != step.state {
			t.Fatalf("state %s", got.State)
		}
	}
	if _, err := runAdminCommand(ts.srv, root.ID, swarmID, "node_drain", `{"name":"managed:brood-proxmox"}`); err == nil {
		t.Fatal("scoped node mutation allowed")
	}
	if pve.stops != 0 || pve.deletes != 0 || pve.creates != 0 {
		t.Fatal("admission-only operation changed guests")
	}
	result, err := runAdminCommand(ts.srv, root.ID, "", "node", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result.Data), `"name":"managed:brood-proxmox"`) {
		t.Fatalf("managed inventory missing: %s", result.Data)
	}
	for _, value := range []string{p.Source.Endpoint, p.Source.Node, p.Source.Network} {
		if value != "" && strings.Contains(string(result.Data), `"`+value+`"`) {
			t.Fatalf("provider configuration leaked: %s", result.Data)
		}
	}
}

func TestAdminCommandProjectScope(t *testing.T) {
	s := adminCommandServer(t)
	for _, name := range []string{"one", "two"} {
		if err := s.store.CreateProject(context.Background(), store.Project{ID: name, Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.store.CreateSwarm(context.Background(), store.Swarm{ID: "scope", ProjectID: "one", OwnerID: "admin", Name: "scope", AtespaceName: "scope", State: "active"}); err != nil {
		t.Fatal(err)
	}
	if _, err := runAdminCommand(s, "admin", "scope", "project_limit", `{"actors":3}`); err != nil {
		t.Fatal(err)
	}
	if _, err := runAdminCommand(s, "admin", "scope", "project_limit", `{"name":"two","actors":4}`); err == nil {
		t.Fatal("cross-project patch allowed")
	}
	one, _ := s.store.GetProjectCapacity(context.Background(), "one")
	two, _ := s.store.GetProjectCapacity(context.Background(), "two")
	if one.MaxActors == nil || *one.MaxActors != 3 || two.MaxActors != nil {
		t.Fatalf("scope violated: %+v %+v", one, two)
	}
}

func TestAdminCommandModelDiscovery(t *testing.T) {
	s := adminCommandServer(t)
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/models" || r.Method != "GET" || r.Header.Get("Authorization") != "Bearer private-model-key" {
			t.Errorf("bad model request %s", r.URL.Path)
		}
		io.WriteString(w, `{"data":[{"id":"installed-small","api_key":"private-model-key","endpoint":"http://private-address.invalid"}],"secret":"private-model-key"}`)
	}))
	defer backend.Close()
	s.adminModels = newAdminModelClient(Config{CLIProxyURL: backend.URL, CLIProxyAPIKey: "private-model-key"})
	result, err := runAdminCommand(s, "ordinary", "", "models", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	if err := json.Unmarshal(result.Data, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Models) != 1 || data.Models[0].ID != "installed-small" {
		t.Fatalf("models: %s", result.Data)
	}
	for _, value := range []string{"private-model-key", "private-address.invalid", "api_key", "endpoint"} {
		if strings.Contains(string(result.Data), value) {
			t.Fatalf("model response leaked %s", value)
		}
	}
	if _, err := runAdminCommand(s, "disabled", "", "models", `{}`); err == nil {
		t.Fatal("disabled model discovery allowed")
	}
	if calls.Load() != 1 {
		t.Fatal("disabled user reached proxy")
	}
}
func TestAdminCommandModelRedirectFailureAndCancellation(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { destinationCalls.Add(1) }))
	defer destination.Close()
	for _, status := range []int{http.StatusFound, http.StatusInternalServerError} {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", destination.URL+"/private-model-key")
			w.WriteHeader(status)
			io.WriteString(w, "private-model-key http://private-address.invalid")
		}))
		client := newAdminModelClient(Config{CLIProxyURL: backend.URL, CLIProxyAPIKey: "private-model-key"})
		_, err := client.models(context.Background())
		backend.Close()
		if err == nil || strings.Contains(err.Error(), "private-") {
			t.Fatalf("unsafe model failure %v", err)
		}
	}
	if destinationCalls.Load() != 0 {
		t.Fatal("model redirect followed")
	}
	client := newAdminModelClient(Config{CLIProxyURL: destination.URL, CLIProxyAPIKey: "private-model-key"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.models(ctx); err == nil {
		t.Fatal("cancellation ignored")
	}
	if destinationCalls.Load() != 0 {
		t.Fatal("cancelled discovery reached destination")
	}
	if client.client.Timeout <= 0 || client.client.Timeout > 10e9 {
		t.Fatal("model timeout is unbounded")
	}
}
