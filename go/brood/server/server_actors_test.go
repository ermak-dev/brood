// Сквозные проверки actor/template/worker/policy API и подключения
// прокси: ateapi подменяется подделкой Control на TLS-слушателе (как
// client_test пакета substrate, но на реальном TCP, чтобы сервер собрал
// клиент из своих настроек ATEAPI_*).
package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"brood.dev/go/brood/proxy"
	"brood.dev/go/brood/substrate"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// ─── подделка Control ──────────────────────────────────────────────────────

// fakeAteapi — минимальная подделка Control для проверок сервера: словари
// сущностей без страниц; пейджинация и предусловия проверены в пакете
// substrate, здесь важен HTTP-слой.
type fakeAteapi struct {
	ateapi.UnimplementedControlServer
	mu                 sync.Mutex
	atespaces          map[string]*ateapi.Atespace
	actors             map[string]*ateapi.Actor
	templates          map[string]*ateapi.ActorTemplate
	workers            map[string]*ateapi.Worker
	policies           map[string]*ateapi.AccessPolicy // "" — глобальная
	namespaceReads     int
	beforeNamespaceGet func()
}

func newFakeAteapi() *fakeAteapi {
	return &fakeAteapi{
		atespaces: map[string]*ateapi.Atespace{},
		actors:    map[string]*ateapi.Actor{},
		templates: map[string]*ateapi.ActorTemplate{},
		workers:   map[string]*ateapi.Worker{},
		policies:  map[string]*ateapi.AccessPolicy{},
	}
}

// fakeMeta — метадория с фиксированными uid/version.
func fakeMeta(name, atespace string) *ateapi.ResourceMetadata {
	return &ateapi.ResourceMetadata{Atespace: atespace, Name: name,
		Uid: "uid-" + name, Version: 7}
}

func grpcErr(code codes.Code, msg string) error { return status.Error(code, msg) }

func (f *fakeAteapi) CreateAtespace(_ context.Context, req *ateapi.CreateAtespaceRequest) (*ateapi.Atespace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	item := req.GetAtespace()
	name := item.GetMetadata().GetName()
	if _, ok := f.atespaces[name]; ok {
		return nil, grpcErr(codes.AlreadyExists, "atespace already exists")
	}
	item.Metadata = fakeMeta(name, "")
	f.atespaces[name] = item
	return item, nil
}

func (f *fakeAteapi) GetAtespace(_ context.Context, req *ateapi.GetAtespaceRequest) (*ateapi.Atespace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.namespaceReads++
	if hook := f.beforeNamespaceGet; hook != nil {
		f.beforeNamespaceGet = nil
		hook()
	}
	item, ok := f.atespaces[req.GetAtespace().GetName()]
	if !ok {
		return nil, grpcErr(codes.NotFound, "atespace not found")
	}
	return item, nil
}

func (f *fakeAteapi) ListAtespaces(_ context.Context, _ *ateapi.ListAtespacesRequest) (*ateapi.ListAtespacesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.namespaceReads++
	items := []*ateapi.Atespace{}
	for _, item := range f.atespaces {
		items = append(items, item)
	}
	return &ateapi.ListAtespacesResponse{Atespaces: items}, nil
}

func (f *fakeAteapi) DeleteAtespace(_ context.Context, req *ateapi.DeleteAtespaceRequest) (*ateapi.Atespace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	item, ok := f.atespaces[req.GetAtespace().GetName()]
	if !ok {
		return nil, grpcErr(codes.NotFound, "atespace not found")
	}
	if uid := req.GetOptions().GetUid(); uid != "" && uid != item.GetMetadata().GetUid() {
		return nil, grpcErr(codes.Aborted, "atespace UID changed")
	}
	name := req.GetAtespace().GetName()
	for _, actor := range f.actors {
		if actor.GetMetadata().GetAtespace() == name {
			return nil, grpcErr(codes.FailedPrecondition, "Atespace is not empty")
		}
	}
	for _, template := range f.templates {
		if template.GetMetadata().GetAtespace() == name {
			return nil, grpcErr(codes.FailedPrecondition, "Atespace is not empty")
		}
	}
	delete(f.atespaces, name)
	return item, nil
}

func (f *fakeAteapi) ListActors(_ context.Context, req *ateapi.ListActorsRequest) (*ateapi.ListActorsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	items := []*ateapi.Actor{}
	for _, item := range f.actors {
		if item.GetMetadata().GetAtespace() == req.GetAtespace() {
			items = append(items, item)
		}
	}
	return &ateapi.ListActorsResponse{Actors: items}, nil
}

func (f *fakeAteapi) GetActor(_ context.Context, req *ateapi.GetActorRequest) (*ateapi.Actor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	item, ok := f.actors[req.GetActor().GetAtespace()+"/"+req.GetActor().GetName()]
	if !ok {
		return nil, grpcErr(codes.NotFound, fmt.Sprintf("actor %s not found", req.GetActor().GetName()))
	}
	return item, nil
}

func (f *fakeAteapi) CreateActor(_ context.Context, req *ateapi.CreateActorRequest) (*ateapi.Actor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	item := req.GetActor()
	key := item.GetMetadata().GetAtespace() + "/" + item.GetMetadata().GetName()
	if _, ok := f.actors[key]; ok {
		return nil, grpcErr(codes.AlreadyExists, "actor already exists")
	}
	item.Metadata.Uid = "uid-" + item.Metadata.Name
	item.Metadata.Version = 7
	f.actors[key] = item
	return item, nil
}

func (f *fakeAteapi) UpdateActor(_ context.Context, req *ateapi.UpdateActorRequest) (*ateapi.Actor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	item := req.GetActor()
	key := item.GetMetadata().GetAtespace() + "/" + item.GetMetadata().GetName()
	if _, ok := f.actors[key]; !ok {
		return nil, grpcErr(codes.NotFound, "actor not found")
	}
	f.actors[key] = item
	return item, nil
}

func (f *fakeAteapi) SuspendActor(_ context.Context, req *ateapi.SuspendActorRequest) (*ateapi.SuspendActorResponse, error) {
	item, err := f.actorOf(req.GetActor())
	if err != nil {
		return nil, err
	}
	return &ateapi.SuspendActorResponse{Actor: item}, nil
}

func (f *fakeAteapi) ResumeActor(_ context.Context, req *ateapi.ResumeActorRequest) (*ateapi.ResumeActorResponse, error) {
	item, err := f.actorOf(req.GetActor())
	if err != nil {
		return nil, err
	}
	return &ateapi.ResumeActorResponse{Actor: item}, nil
}

func (f *fakeAteapi) DeleteActor(_ context.Context, req *ateapi.DeleteActorRequest) (*ateapi.Actor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := req.GetActor().GetAtespace() + "/" + req.GetActor().GetName()
	item, ok := f.actors[key]
	if !ok {
		return nil, grpcErr(codes.NotFound, "actor not found")
	}
	delete(f.actors, key)
	return item, nil
}

func (f *fakeAteapi) actorOf(ref *ateapi.ObjectRef) (*ateapi.Actor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	item, ok := f.actors[ref.GetAtespace()+"/"+ref.GetName()]
	if !ok {
		return nil, grpcErr(codes.NotFound, "actor not found")
	}
	return item, nil
}

func (f *fakeAteapi) ListActorTemplates(_ context.Context, req *ateapi.ListActorTemplatesRequest) (*ateapi.ListActorTemplatesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	items := []*ateapi.ActorTemplate{}
	for _, item := range f.templates {
		if item.GetMetadata().GetAtespace() == req.GetAtespace() {
			items = append(items, item)
		}
	}
	return &ateapi.ListActorTemplatesResponse{ActorTemplates: items}, nil
}

func (f *fakeAteapi) GetActorTemplate(_ context.Context, req *ateapi.GetActorTemplateRequest) (*ateapi.ActorTemplate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	item, ok := f.templates[req.GetActorTemplate().GetAtespace()+"/"+req.GetActorTemplate().GetName()]
	if !ok {
		return nil, grpcErr(codes.NotFound, "actor template not found")
	}
	return item, nil
}

func (f *fakeAteapi) CreateActorTemplate(_ context.Context, req *ateapi.CreateActorTemplateRequest) (*ateapi.ActorTemplate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	item := req.GetActorTemplate()
	key := item.GetMetadata().GetAtespace() + "/" + item.GetMetadata().GetName()
	if _, ok := f.templates[key]; ok {
		return nil, grpcErr(codes.AlreadyExists, "actor template already exists")
	}
	item.Metadata.Uid = "uid-" + item.Metadata.Name
	item.Metadata.Version = 7
	f.templates[key] = item
	return item, nil
}

func (f *fakeAteapi) DeleteActorTemplate(_ context.Context, req *ateapi.DeleteActorTemplateRequest) (*ateapi.ActorTemplate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := req.GetActorTemplate().GetAtespace() + "/" + req.GetActorTemplate().GetName()
	item, ok := f.templates[key]
	if !ok {
		return nil, grpcErr(codes.NotFound, "actor template not found")
	}
	delete(f.templates, key)
	return item, nil
}

func (f *fakeAteapi) ListWorkers(_ context.Context, _ *ateapi.ListWorkersRequest) (*ateapi.ListWorkersResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	items := []*ateapi.Worker{}
	for _, item := range f.workers {
		items = append(items, item)
	}
	return &ateapi.ListWorkersResponse{Workers: items}, nil
}

func (f *fakeAteapi) GetWorker(_ context.Context, req *ateapi.GetWorkerRequest) (*ateapi.Worker, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	item, ok := f.workers[req.GetWorker().GetName()]
	if !ok {
		return nil, grpcErr(codes.NotFound, "worker not found")
	}
	return item, nil
}

func (f *fakeAteapi) DrainWorker(_ context.Context, req *ateapi.DrainWorkerRequest) (*ateapi.DrainWorkerResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	item, ok := f.workers[req.GetWorker().GetName()]
	if !ok {
		return nil, grpcErr(codes.NotFound, "worker not found")
	}
	return &ateapi.DrainWorkerResponse{Worker: item}, nil
}

func (f *fakeAteapi) GetGlobalAccessPolicy(_ context.Context, req *ateapi.GetGlobalAccessPolicyRequest) (*ateapi.AccessPolicy, error) {
	if req.GetName() != substrate.PolicyName {
		return nil, grpcErr(codes.InvalidArgument, "access policy name must be default")
	}
	return f.policyOf("")
}

func (f *fakeAteapi) CreateGlobalAccessPolicy(_ context.Context, req *ateapi.CreateGlobalAccessPolicyRequest) (*ateapi.AccessPolicy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.policies[""]; ok {
		return nil, grpcErr(codes.AlreadyExists, "global access policy already exists")
	}
	f.policies[""] = req.GetAccessPolicy()
	return req.GetAccessPolicy(), nil
}

func (f *fakeAteapi) UpdateGlobalAccessPolicy(_ context.Context, req *ateapi.UpdateGlobalAccessPolicyRequest) (*ateapi.AccessPolicy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.policies[""]; !ok {
		return nil, grpcErr(codes.NotFound, "global access policy not found")
	}
	f.policies[""] = req.GetAccessPolicy()
	return req.GetAccessPolicy(), nil
}

func (f *fakeAteapi) GetAtespaceAccessPolicy(_ context.Context, req *ateapi.GetAtespaceAccessPolicyRequest) (*ateapi.AccessPolicy, error) {
	if req.GetName() != substrate.PolicyName {
		return nil, grpcErr(codes.InvalidArgument, "access policy name must be default")
	}
	return f.policyOf(req.GetAtespace().GetName())
}

func (f *fakeAteapi) CreateAtespaceAccessPolicy(_ context.Context, req *ateapi.CreateAtespaceAccessPolicyRequest) (*ateapi.AccessPolicy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	scope := req.GetAtespace().GetName()
	if _, ok := f.policies[scope]; ok {
		return nil, grpcErr(codes.AlreadyExists, "atespace access policy already exists")
	}
	f.policies[scope] = req.GetAccessPolicy()
	return req.GetAccessPolicy(), nil
}

func (f *fakeAteapi) UpdateAtespaceAccessPolicy(_ context.Context, req *ateapi.UpdateAtespaceAccessPolicyRequest) (*ateapi.AccessPolicy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	scope := req.GetAtespace().GetName()
	if _, ok := f.policies[scope]; !ok {
		return nil, grpcErr(codes.NotFound, "atespace access policy not found")
	}
	f.policies[scope] = req.GetAccessPolicy()
	return req.GetAccessPolicy(), nil
}

func (f *fakeAteapi) DeleteAtespaceAccessPolicy(_ context.Context, req *ateapi.DeleteAtespaceAccessPolicyRequest) (*ateapi.AccessPolicy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	scope := req.GetAtespace().GetName()
	item, ok := f.policies[scope]
	if !ok {
		return nil, grpcErr(codes.NotFound, "atespace access policy not found")
	}
	delete(f.policies, scope)
	return item, nil
}

func (f *fakeAteapi) policyOf(scope string) (*ateapi.AccessPolicy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	item, ok := f.policies[scope]
	if !ok {
		return nil, grpcErr(codes.NotFound, "access policy not found")
	}
	return item, nil
}

// setRunning помечает актора запущенным: worker_assignment задан — так
// выглядит routable-актор для резолвера.
func (f *fakeAteapi) setRunning(atespace, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := atespace + "/" + name
	actor := f.actors[key]
	if actor == nil {
		actor = &ateapi.Actor{Metadata: fakeMeta(name, atespace)}
		f.actors[key] = actor
	}
	actor.Status = &ateapi.ActorStatus{
		State:            2, // RUNNING
		WorkerAssignment: &ateapi.WorkerAssignment{WorkerPodIps: []string{"10.0.0.1"}},
	}
}

// ─── обвязка: сервер с настроенным Substrate ───────────────────────────────

// selfSignFake выписывает самоподписанный сертификат на имя сервиса — как
// корень ClusterTrustBundle, которым подписан ateapi.
func selfSignFake(t *testing.T, name string) (tls.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		DNSNames:              []string{name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	pemData := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pemData
}

// writeFileFake кладёт файл во временный каталог проверки.
func writeFileFake(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// substrateServer — тестовый сервер с настроенным Substrate-клиентом,
// который говорит с подделкой Control на локальном TLS-слушателе.
type substrateServer struct {
	*testServer
	fake *fakeAteapi
	grpc *grpc.Server
}

// newSubstrateServer поднимает подделку Control на TCP-слушателе с TLS
// на Audience и собирает сервер с настройками ATEAPI_*: клиент едет тот
// же, что в бою, включая сверку имени сервиса на нестандартном endpoint.
func newSubstrateServer(t *testing.T, fake *fakeAteapi, configure ...func(*Config)) *substrateServer {
	t.Helper()
	tokenFile := writeFileFake(t, "token", "test-token\n")
	caFile := writeFileFake(t, "ca.pem", "")
	cert, caPEM := selfSignFake(t, substrate.Audience)
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}})))
	ateapi.RegisterControlServer(server, fake)
	go server.Serve(listener)
	t.Cleanup(server.Stop)

	cfg := Config{
		DBPath:         t.TempDir() + "/brood.db",
		AteapiToken:    tokenFile,
		AteapiCA:       caFile,
		AteapiEndpoint: listener.Addr().String(),
	}
	for _, configure := range configure {
		configure(&cfg)
	}
	srv, err := New(nil2ctx(), cfg, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	base := &testServer{t: t, srv: srv}
	base.http = httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		base.http.Close()
		srv.Close()
	})
	return &substrateServer{testServer: base, fake: fake, grpc: server}
}

// actorsSetup — администратор, два участника одного проекта, активный рой
// alice. Возвращает клиентов и ID роя.
func actorsSetup(t *testing.T, ts *testServer) (admin, alice, bob *http.Client, swarmID string) {
	t.Helper()
	ts.bootstrapAdmin("root", "root-password-1")
	admin = newClient(t)
	ts.login(admin, "root", "root-password-1")
	for _, name := range []string{"alice", "bob"} {
		resp, data, _ := ts.do(admin, "POST", "/api/admin/users", ts.csrf(admin),
			map[string]any{"username": name, "password": name + "-password-1"})
		expectStatus(t, "create user "+name, resp, data, http.StatusCreated)
	}
	resp, data, _ := ts.do(admin, "POST", "/api/projects", ts.csrf(admin),
		map[string]string{"name": "proj"})
	expectStatus(t, "create project", resp, data, http.StatusCreated)
	projectID, _ := data["id"].(string)
	for _, name := range []string{"alice", "bob"} {
		resp, data, _ = ts.do(admin, "POST", "/api/projects/"+projectID+"/members",
			ts.csrf(admin), map[string]string{"username": name})
		expectStatus(t, "add member "+name, resp, data, http.StatusNoContent)
	}
	alice = newClient(t)
	ts.login(alice, "alice", "alice-password-1")
	resp, data, _ = ts.do(alice, "POST", "/api/projects/"+projectID+"/swarms",
		ts.csrf(alice), map[string]string{"name": "alpha"})
	expectStatus(t, "alice swarm", resp, data, http.StatusCreated)
	if data["state"] != "active" {
		t.Logf("swarm state: %v", data["state"])
	}
	swarmID, _ = data["id"].(string)
	bob = newClient(t)
	ts.login(bob, "bob", "bob-password-1")
	return admin, alice, bob, swarmID
}

// ─── проверки: акторы ──────────────────────────────────────────────────────

// TestSwarmActorsLifecycle — список, создание (atespace из роя, не из тела),
// чтение, update, suspend/resume, удаление; изоляция чужого роя и анонима.
func TestSwarmActorsLifecycle(t *testing.T) {
	fake := newFakeAteapi()
	ts := newSubstrateServer(t, fake)
	_, alice, bob, swarmID := actorsSetup(t, ts.testServer)

	resp, data, _ := ts.do(alice, "GET", "/api/swarms/"+swarmID+"/actors", "", nil)
	expectStatus(t, "empty list", resp, data, http.StatusOK)
	if results, ok := data["results"].([]any); !ok || len(results) != 0 {
		t.Fatalf("empty list: %v", data)
	}

	swarm, err := ts.srv.store.GetSwarm(nil2ctx(), swarmID)
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	for _, name := range []string{"tpl", "tpl2"} {
		fake.templates[swarm.AtespaceName+"/"+name] = inventoryTemplate(swarm.AtespaceName, name, "uid-"+name, ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED)
	}
	fake.mu.Unlock()

	// Atespace в теле игнорируется: сервер ставит имя области роя.
	resp, data, _ = ts.do(alice, "POST", "/api/swarms/"+swarmID+"/actors", ts.csrf(alice),
		map[string]any{"metadata": map[string]any{"name": "claude", "atespace": "evil"},
			"actorTemplate": map[string]any{"name": "tpl", "atespace": "evil"},
			"sourceTag":     map[string]any{"name": "seed", "atespace": "evil"}})
	expectStatus(t, "create actor", resp, data, http.StatusCreated)
	meta, _ := data["metadata"].(map[string]any)
	swarm, err = ts.srv.store.GetSwarm(nil2ctx(), swarmID)
	if err != nil {
		t.Fatalf("get swarm: %v", err)
	}
	if meta["name"] != "claude" || meta["atespace"] != swarm.AtespaceName {
		t.Fatalf("create actor metadata: %v, want atespace %s", meta, swarm.AtespaceName)
	}
	templateRef, _ := data["actorTemplate"].(map[string]any)
	if templateRef["atespace"] != swarm.AtespaceName {
		t.Fatalf("template reference escaped swarm: %v", templateRef)
	}
	tagRef, _ := data["sourceTag"].(map[string]any)
	if tagRef["atespace"] != swarm.AtespaceName {
		t.Fatalf("tag reference escaped swarm: %v", tagRef)
	}
	// Подделка хранит актора в области роя.
	fake.mu.Lock()
	_, stored := fake.actors[swarm.AtespaceName+"/claude"]
	fake.mu.Unlock()
	if !stored {
		t.Fatalf("actor stored outside swarm atespace")
	}

	resp, data, _ = ts.do(alice, "GET", "/api/swarms/"+swarmID+"/actors/claude", "", nil)
	expectStatus(t, "get actor", resp, data, http.StatusOK)

	resp, data, _ = ts.do(alice, "PUT", "/api/swarms/"+swarmID+"/actors/claude", ts.csrf(alice),
		map[string]any{"metadata": map[string]any{"name": "claude", "atespace": "evil"},
			"actorTemplate": map[string]any{"name": "tpl2"}})
	expectStatus(t, "update actor", resp, data, http.StatusOK)
	meta, _ = data["metadata"].(map[string]any)
	if meta["atespace"] != swarm.AtespaceName {
		t.Fatalf("update actor atespace: %v", meta)
	}

	for _, action := range []string{"suspend", "resume"} {
		resp, data, _ = ts.do(alice, "POST", "/api/swarms/"+swarmID+"/actors/claude",
			ts.csrf(alice), map[string]string{"action": action})
		expectStatus(t, action+" actor", resp, data, http.StatusOK)
	}
	resp, data, _ = ts.do(alice, "POST", "/api/swarms/"+swarmID+"/actors/claude",
		ts.csrf(alice), map[string]string{"action": "explode"})
	expectStatus(t, "bad action", resp, data, http.StatusBadRequest)

	// Неизвестное поле тела — 400 с причиной, не 500.
	resp, data, _ = ts.do(alice, "POST", "/api/swarms/"+swarmID+"/actors", ts.csrf(alice),
		map[string]any{"metadata": map[string]any{"name": "x"}, "evil": true})
	expectStatus(t, "unknown field", resp, data, http.StatusBadRequest)
	if detail, _ := data["detail"].(string); !strings.Contains(detail, "unknown") {
		t.Fatalf("unknown field detail: %v", data["detail"])
	}

	resp, data, _ = ts.do(alice, "DELETE", "/api/swarms/"+swarmID+"/actors/claude",
		ts.csrf(alice), nil)
	expectStatus(t, "delete actor", resp, data, http.StatusNoContent)

	// Членство не открывает чужой рой: 404, существование не раскрывается.
	resp, data, _ = ts.do(bob, "GET", "/api/swarms/"+swarmID+"/actors", "", nil)
	expectStatus(t, "bob lists alice actors", resp, data, http.StatusNotFound)
	resp, data, _ = ts.do(bob, "POST", "/api/swarms/"+swarmID+"/actors", ts.csrf(bob),
		map[string]any{"metadata": map[string]any{"name": "spy"}})
	expectStatus(t, "bob creates in alice swarm", resp, data, http.StatusNotFound)

	// Аноним — 401.
	anon := newClient(t)
	resp, data, _ = ts.do(anon, "GET", "/api/swarms/"+swarmID+"/actors", "", nil)
	expectStatus(t, "anonymous actors", resp, data, http.StatusUnauthorized)
}

// TestSwarmTemplates — шаблоны: create/list/get/delete, изоляция роя.
func TestSwarmTemplates(t *testing.T) {
	fake := newFakeAteapi()
	ts := newSubstrateServer(t, fake)
	_, alice, bob, swarmID := actorsSetup(t, ts.testServer)
	swarm, _ := ts.srv.store.GetSwarm(nil2ctx(), swarmID)

	resp, data, _ := ts.do(alice, "POST", "/api/swarms/"+swarmID+"/templates", ts.csrf(alice),
		map[string]any{"metadata": map[string]any{"name": "tpl", "atespace": "evil"},
			"containers":     []any{map[string]string{"name": "main", "image": "busybox"}},
			"resources":      map[string]any{"limits": map[string]string{"cpuMillicores": "1000", "memoryBytes": "1048576"}},
			"snapshotConfig": map[string]any{"goldenSnapshotConfig": map[string]string{"mode": "GOLDEN_SNAPSHOT_MODE_DISABLED"}}})
	expectStatus(t, "create template", resp, data, http.StatusCreated)
	meta, _ := data["metadata"].(map[string]any)
	if meta["atespace"] != swarm.AtespaceName {
		t.Fatalf("template atespace: %v", meta)
	}

	resp, data, _ = ts.do(alice, "GET", "/api/swarms/"+swarmID+"/templates", "", nil)
	expectStatus(t, "list templates", resp, data, http.StatusOK)
	if results, _ := data["results"].([]any); len(results) != 1 {
		t.Fatalf("templates: %v", data)
	}

	resp, data, _ = ts.do(alice, "GET", "/api/swarms/"+swarmID+"/templates/tpl", "", nil)
	expectStatus(t, "get template", resp, data, http.StatusOK)

	resp, data, _ = ts.do(bob, "GET", "/api/swarms/"+swarmID+"/templates", "", nil)
	expectStatus(t, "bob lists templates", resp, data, http.StatusNotFound)

	resp, data, _ = ts.do(alice, "DELETE", "/api/swarms/"+swarmID+"/templates/tpl",
		ts.csrf(alice), nil)
	expectStatus(t, "delete template", resp, data, http.StatusNoContent)
}

// TestWorkersAdminOnly — воркеры установки только админу.
func TestWorkersAdminOnly(t *testing.T) {
	fake := newFakeAteapi()
	fake.mu.Lock()
	fake.workers["worker-1"] = &ateapi.Worker{Metadata: fakeMeta("worker-1", ""), WorkerPool: "pool"}
	fake.mu.Unlock()
	ts := newSubstrateServer(t, fake)
	admin, alice, _, _ := actorsSetup(t, ts.testServer)

	resp, data, _ := ts.do(alice, "GET", "/api/workers", "", nil)
	expectStatus(t, "non-admin workers", resp, data, http.StatusForbidden)
	expectDetail(t, "non-admin workers", data, "administrator access required")

	resp, data, _ = ts.do(admin, "GET", "/api/workers", "", nil)
	expectStatus(t, "admin workers", resp, data, http.StatusOK)
	if results, _ := data["results"].([]any); len(results) != 1 {
		t.Fatalf("workers: %v", data)
	}

	resp, data, _ = ts.do(admin, "GET", "/api/workers/worker-1", "", nil)
	expectStatus(t, "get worker", resp, data, http.StatusOK)
	resp, data, _ = ts.do(admin, "GET", "/api/workers/missing", "", nil)
	expectStatus(t, "missing worker", resp, data, http.StatusNotFound)

	resp, data, _ = ts.do(admin, "POST", "/api/workers/worker-1", ts.csrf(admin),
		map[string]string{"action": "drain"})
	expectStatus(t, "drain worker", resp, data, http.StatusOK)
	resp, data, _ = ts.do(admin, "POST", "/api/workers/worker-1", ts.csrf(admin),
		map[string]string{"action": "nuke"})
	expectStatus(t, "bad worker action", resp, data, http.StatusBadRequest)

	anon := newClient(t)
	resp, data, _ = ts.do(anon, "GET", "/api/workers", "", nil)
	expectStatus(t, "anonymous workers", resp, data, http.StatusUnauthorized)
}

// TestSwarmPolicy — политика доступа роя: get/create/update/delete.
func TestSwarmPolicy(t *testing.T) {
	fake := newFakeAteapi()
	ts := newSubstrateServer(t, fake)
	admin, _, bob, swarmID := actorsSetup(t, ts.testServer)

	// Полный CRUD сохранён для администратора, даже в рое другого владельца.
	resp, data, _ := ts.do(admin, "GET", "/api/swarms/"+swarmID+"/policy", "", nil)
	expectStatus(t, "missing policy", resp, data, http.StatusNotFound)

	bindings := []any{map[string]any{"role": "viewer", "members": []string{"user:someone"}}}
	resp, data, _ = ts.do(admin, "POST", "/api/swarms/"+swarmID+"/policy", ts.csrf(admin),
		map[string]any{"bindings": bindings})
	expectStatus(t, "create policy", resp, data, http.StatusCreated)
	if meta, _ := data["metadata"].(map[string]any); meta["name"] != "default" {
		t.Fatalf("policy name: %v", meta)
	}

	resp, data, _ = ts.do(admin, "GET", "/api/swarms/"+swarmID+"/policy", "", nil)
	expectStatus(t, "get policy", resp, data, http.StatusOK)

	resp, data, _ = ts.do(admin, "PUT", "/api/swarms/"+swarmID+"/policy", ts.csrf(admin),
		map[string]any{"bindings": bindings})
	expectStatus(t, "update policy", resp, data, http.StatusOK)

	resp, data, _ = ts.do(bob, "GET", "/api/swarms/"+swarmID+"/policy", "", nil)
	expectStatus(t, "bob reads policy", resp, data, http.StatusForbidden)

	resp, data, _ = ts.do(admin, "DELETE", "/api/swarms/"+swarmID+"/policy",
		ts.csrf(admin), nil)
	expectStatus(t, "delete policy", resp, data, http.StatusNoContent)
	resp, data, _ = ts.do(admin, "GET", "/api/swarms/"+swarmID+"/policy", "", nil)
	expectStatus(t, "policy after delete", resp, data, http.StatusNotFound)
}

// TestGlobalPolicyAdminOnly — глобальная политика только админу.
func TestGlobalPolicyAdminOnly(t *testing.T) {
	fake := newFakeAteapi()
	ts := newSubstrateServer(t, fake)
	admin, alice, _, _ := actorsSetup(t, ts.testServer)

	resp, data, _ := ts.do(alice, "GET", "/api/policy/global", "", nil)
	expectStatus(t, "non-admin global policy", resp, data, http.StatusForbidden)

	resp, data, _ = ts.do(admin, "GET", "/api/policy/global", "", nil)
	expectStatus(t, "missing global policy", resp, data, http.StatusNotFound)

	bindings := []any{map[string]any{"role": "viewer", "members": []string{"user:someone"}}}
	resp, data, _ = ts.do(admin, "POST", "/api/policy/global", ts.csrf(admin),
		map[string]any{"bindings": bindings})
	expectStatus(t, "create global policy", resp, data, http.StatusCreated)
	resp, data, _ = ts.do(admin, "PUT", "/api/policy/global", ts.csrf(admin),
		map[string]any{"bindings": bindings})
	expectStatus(t, "update global policy", resp, data, http.StatusOK)
}

// TestActorsSubstrateOutage — недоступный ateapi — 503, не 500.
func TestActorsSubstrateOutage(t *testing.T) {
	fake := newFakeAteapi()
	ts := newSubstrateServer(t, fake)
	_, alice, _, swarmID := actorsSetup(t, ts.testServer)
	ts.grpc.Stop()

	resp, data, _ := ts.do(alice, "GET", "/api/swarms/"+swarmID+"/actors", "", nil)
	expectStatus(t, "substrate down", resp, data, http.StatusServiceUnavailable)
}

// TestActorsSubstrateUnconfigured — без настроек ATEAPI — 503 с именами
// настроек.
func TestActorsSubstrateUnconfigured(t *testing.T) {
	ts := newTestServer(t)
	ts.bootstrapAdmin("root", "root-password-1")
	admin := newClient(t)
	ts.login(admin, "root", "root-password-1")
	resp, data, _ := ts.do(admin, "POST", "/api/admin/users", ts.csrf(admin),
		map[string]any{"username": "alice", "password": "alice-password-1"})
	expectStatus(t, "create user", resp, data, http.StatusCreated)
	resp, data, _ = ts.do(admin, "POST", "/api/projects", ts.csrf(admin),
		map[string]string{"name": "proj"})
	expectStatus(t, "create project", resp, data, http.StatusCreated)
	projectID, _ := data["id"].(string)
	resp, data, _ = ts.do(admin, "POST", "/api/projects/"+projectID+"/members",
		ts.csrf(admin), map[string]string{"username": "alice"})
	expectStatus(t, "add member", resp, data, http.StatusNoContent)

	alice := newClient(t)
	ts.login(alice, "alice", "alice-password-1")
	// Провижинер поддельный; управляющий actor API при этом не настроен.
	resp, data, _ = ts.do(alice, "POST", "/api/projects/"+projectID+"/swarms",
		ts.csrf(alice), map[string]string{"name": "alpha"})
	expectStatus(t, "swarm create", resp, data, http.StatusCreated)
	swarmID, _ := data["id"].(string)

	resp, data, _ = ts.do(alice, "GET", "/api/swarms/"+swarmID+"/actors", "", nil)
	expectStatus(t, "unconfigured actors", resp, data, http.StatusServiceUnavailable)
	if detail, _ := data["detail"].(string); !strings.Contains(detail, "ATEAPI_TOKEN") {
		t.Fatalf("unconfigured detail: %v", data["detail"])
	}
}

// ─── проверки: прокси и резолвер ───────────────────────────────────────────

// TestProxyMountWiring — прокси подключён под /api/swarms/{id}/proxy/...:
// аноним 401, чужой рой 404, неизвестный и не-запущенный актор 404.
func TestProxyMountWiring(t *testing.T) {
	fake := newFakeAteapi()
	ts := newSubstrateServer(t, fake)
	_, alice, bob, swarmID := actorsSetup(t, ts.testServer)
	base := "/api/swarms/" + swarmID + "/proxy/actors/"

	anon := newClient(t)
	resp, data, _ := ts.do(anon, "GET", base+"claude", "", nil)
	expectStatus(t, "anonymous proxy", resp, data, http.StatusUnauthorized)

	resp, data, _ = ts.do(bob, "GET", base+"claude", "", nil)
	expectStatus(t, "foreign swarm proxy", resp, data, http.StatusNotFound)

	resp, data, _ = ts.do(alice, "GET", base+"ghost", "", nil)
	expectStatus(t, "unknown actor proxy", resp, data, http.StatusNotFound)

	fake.setRunning(swarmAtespace(t, ts.srv, swarmID), "idle")
	resp, data, _ = ts.do(alice, "GET", base+"idle", "", nil)
	expectStatus(t, "router is not configured", resp, data, http.StatusServiceUnavailable)

	// Несуществующий рой — 404.
	resp, data, _ = ts.do(alice, "GET", "/api/swarms/none/proxy/actors/x", "", nil)
	expectStatus(t, "unknown swarm proxy", resp, data, http.StatusNotFound)
}

// swarmAtespace достаёт имя области роя.
func swarmAtespace(t *testing.T, srv *Server, swarmID string) string {
	t.Helper()
	sw, err := srv.store.GetSwarm(nil2ctx(), swarmID)
	if err != nil {
		t.Fatalf("get swarm: %v", err)
	}
	return sw.AtespaceName
}

// TestSubstrateResolver — резолвер: неизвестный и не-запущенный актор —
// ErrUnknownActor, без ATENET_ROUTER — 503 с именем настройки, с адресом —
// https-URL router.
func TestSubstrateResolver(t *testing.T) {
	fake := newFakeAteapi()
	ts := newSubstrateServer(t, fake)
	atespace := "swarm-resolver"
	fake.mu.Lock()
	fake.actors[atespace+"/idle"] = &ateapi.Actor{Metadata: fakeMeta("idle", atespace)}
	fake.mu.Unlock()
	fake.setRunning(atespace, "busy")
	client := ts.srv.ateapi
	if client == nil {
		t.Fatal("server has no substrate client")
	}

	res := NewSubstrateResolver(client, "")
	if _, err := res.ActorUpstream(atespace, "ghost"); !errors.Is(err, proxy.ErrUnknownActor) {
		t.Fatalf("unknown actor: %v", err)
	}
	suspended := NewSubstrateResolver(client, "router.test:443")
	if upstream, err := suspended.ActorUpstream(atespace, "idle"); err != nil || upstream.Host != "router.test:443" {
		t.Fatalf("suspended actor must reach router: upstream=%v err=%v", upstream, err)
	}
	_, err := res.ActorUpstream(atespace, "busy")
	var apiErr *substrate.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusServiceUnavailable ||
		!strings.Contains(apiErr.Detail, "ATENET_ROUTER") {
		t.Fatalf("running actor without router: %v", err)
	}

	routed := NewSubstrateResolver(client, "atenet-router.ate-system.svc:443")
	upstream, err := routed.ActorUpstream(atespace, "busy")
	if err != nil {
		t.Fatalf("running actor with router: %v", err)
	}
	if upstream.Scheme != "https" || upstream.Host != "atenet-router.ate-system.svc:443" {
		t.Fatalf("upstream: %v", upstream)
	}

	// Не настроенный Substrate — 503 с именами настроек.
	unconfigured := NewSubstrateResolver(nil, "x:443")
	_, err = unconfigured.ActorUpstream(atespace, "busy")
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusServiceUnavailable ||
		!strings.Contains(apiErr.Detail, "ATEAPI_TOKEN") {
		t.Fatalf("unconfigured resolver: %v", err)
	}
}

// TestOldSubstratePathsGone — сквозные пути Python-сервера больше не
// отвечают 501: маршрута нет.
func TestOldSubstratePathsGone(t *testing.T) {
	ts := newTestServer(t)
	client := newClient(t)
	for _, path := range []string{"/api/projects/p1/actors", "/api/workers", "/api/policy/global"} {
		resp, data, _ := ts.do(client, "GET", path, "", nil)
		if resp.StatusCode == http.StatusNotImplemented {
			t.Fatalf("stub still mounted at %s", path)
		}
		_ = data
	}
}
