package substrate

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

// ─── подделка Control ──────────────────────────────────────────────────────

// отказ по имени RPC: куда и с каким кодом упасть.
type rpcFailure struct {
	code codes.Code
	msg  string
}

// fakeControl хранит сущности в словарях: страницы, отказы по RPC, запись
// токенов авторизации. Хватает на проверки клиента без живого ateapi.
type fakeControl struct {
	ateapi.UnimplementedControlServer
	mu        sync.Mutex
	auths     []string // значения authorization по вызовам, по порядку
	listCalls []string // page_token каждого List-запроса
	page      int      // размер страницы List-ответов
	atespaces map[string]*ateapi.Atespace
	actors    map[string]*ateapi.Actor
	templates map[string]*ateapi.ActorTemplate
	workers   map[string]*ateapi.Worker
	policies  map[string]*ateapi.AccessPolicy // "" -- глобальная
	egress    map[string]*ateapi.EgressPolicy // "space/actor"
	failures  map[string]rpcFailure           // имя RPC -- отказ
	calls     []string                        // имена вызванных RPC
	deleteUID string
}

func newFake() *fakeControl {
	return &fakeControl{
		page:      2,
		atespaces: map[string]*ateapi.Atespace{},
		actors:    map[string]*ateapi.Actor{},
		templates: map[string]*ateapi.ActorTemplate{},
		workers:   map[string]*ateapi.Worker{},
		policies:  map[string]*ateapi.AccessPolicy{},
		egress:    map[string]*ateapi.EgressPolicy{},
		failures:  map[string]rpcFailure{},
	}
}

// enter фиксирует вызов, токен и отказ; true -- можно работать дальше.
func (f *fakeControl) enter(ctx context.Context, rpc string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, rpc)
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		f.auths = append(f.auths, strings.Join(md.Get("authorization"), ","))
	}
	if failure, ok := f.failures[rpc]; ok {
		return status.Error(failure.code, failure.msg)
	}
	return nil
}

// pageOut вырезает страницу items по токену-смещению.
func pageOut[T any](items []T, opts *ateapi.ListOptions, page int) ([]T, string) {
	start := 0
	if opts != nil && opts.PageToken != "" {
		start, _ = strconv.Atoi(opts.PageToken)
	}
	end := start + page
	if end > len(items) {
		end = len(items)
	}
	next := ""
	if end < len(items) {
		next = strconv.Itoa(end)
	}
	return items[start:end], next
}

func meta(name, atespace string) *ateapi.ResourceMetadata {
	return &ateapi.ResourceMetadata{Atespace: atespace, Name: name,
		Uid: "uid-" + name, Version: 7}
}

func (f *fakeControl) ListAtespaces(ctx context.Context, req *ateapi.ListAtespacesRequest) (*ateapi.ListAtespacesResponse, error) {
	if err := f.enter(ctx, "ListAtespaces"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls = append(f.listCalls, req.GetOptions().GetPageToken())
	items := make([]*ateapi.Atespace, 0, len(f.atespaces))
	for _, name := range sortedKeys(f.atespaces) {
		items = append(items, f.atespaces[name])
	}
	page, next := pageOut(items, req.GetOptions(), f.page)
	return &ateapi.ListAtespacesResponse{Atespaces: page, NextPageToken: next}, nil
}

func (f *fakeControl) GetAtespace(ctx context.Context, req *ateapi.GetAtespaceRequest) (*ateapi.Atespace, error) {
	if err := f.enter(ctx, "GetAtespace"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	name := req.GetAtespace().GetName()
	item, ok := f.atespaces[name]
	if !ok {
		return nil, status.Error(codes.NotFound, fmt.Sprintf("atespace %s not found", name))
	}
	return item, nil
}

func (f *fakeControl) CreateAtespace(ctx context.Context, req *ateapi.CreateAtespaceRequest) (*ateapi.Atespace, error) {
	if err := f.enter(ctx, "CreateAtespace"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	item := req.GetAtespace()
	name := item.GetMetadata().GetName()
	if _, ok := f.atespaces[name]; ok {
		return nil, status.Error(codes.AlreadyExists, fmt.Sprintf("atespace %s already exists", name))
	}
	f.atespaces[name] = item
	return item, nil
}

func (f *fakeControl) DeleteAtespace(ctx context.Context, req *ateapi.DeleteAtespaceRequest) (*ateapi.Atespace, error) {
	if err := f.enter(ctx, "DeleteAtespace"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	item, ok := f.atespaces[req.GetAtespace().GetName()]
	if !ok {
		return nil, status.Error(codes.NotFound, "atespace not found")
	}
	f.deleteUID = req.GetOptions().GetUid()
	delete(f.atespaces, req.GetAtespace().GetName())
	return item, nil
}

func (f *fakeControl) ListActors(ctx context.Context, req *ateapi.ListActorsRequest) (*ateapi.ListActorsResponse, error) {
	if err := f.enter(ctx, "ListActors"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls = append(f.listCalls, req.GetOptions().GetPageToken())
	var items []*ateapi.Actor
	for _, key := range sortedKeys(f.actors) {
		if f.actors[key].GetMetadata().GetAtespace() == req.GetAtespace() {
			items = append(items, f.actors[key])
		}
	}
	page, next := pageOut(items, req.GetOptions(), f.page)
	return &ateapi.ListActorsResponse{Actors: page, NextPageToken: next}, nil
}

func (f *fakeControl) GetActor(ctx context.Context, req *ateapi.GetActorRequest) (*ateapi.Actor, error) {
	if err := f.enter(ctx, "GetActor"); err != nil {
		return nil, err
	}
	return f.actor("GetActor", req.GetActor())
}

func (f *fakeControl) CreateActor(ctx context.Context, req *ateapi.CreateActorRequest) (*ateapi.Actor, error) {
	if err := f.enter(ctx, "CreateActor"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	item := req.GetActor()
	key := item.GetMetadata().GetAtespace() + "/" + item.GetMetadata().GetName()
	if _, ok := f.actors[key]; ok {
		return nil, status.Error(codes.AlreadyExists, "actor already exists")
	}
	f.actors[key] = item
	return item, nil
}

func (f *fakeControl) UpdateActor(ctx context.Context, req *ateapi.UpdateActorRequest) (*ateapi.Actor, error) {
	if err := f.enter(ctx, "UpdateActor"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	item := req.GetActor()
	key := item.GetMetadata().GetAtespace() + "/" + item.GetMetadata().GetName()
	current, ok := f.actors[key]
	if !ok {
		return nil, status.Error(codes.NotFound, "actor not found")
	}
	if current.GetMetadata().GetUid() != item.GetMetadata().GetUid() ||
		current.GetMetadata().GetVersion() != item.GetMetadata().GetVersion() {
		return nil, status.Error(codes.Aborted, "the actor has been modified; reload before retrying")
	}
	f.actors[key] = item
	return item, nil
}

func (f *fakeControl) SuspendActor(ctx context.Context, req *ateapi.SuspendActorRequest) (*ateapi.SuspendActorResponse, error) {
	if err := f.enter(ctx, "SuspendActor"); err != nil {
		return nil, err
	}
	item, err := f.actor("SuspendActor", req.GetActor())
	if err != nil {
		return nil, err
	}
	return &ateapi.SuspendActorResponse{Actor: item}, nil
}

func (f *fakeControl) ResumeActor(ctx context.Context, req *ateapi.ResumeActorRequest) (*ateapi.ResumeActorResponse, error) {
	if err := f.enter(ctx, "ResumeActor"); err != nil {
		return nil, err
	}
	item, err := f.actor("ResumeActor", req.GetActor())
	if err != nil {
		return nil, err
	}
	return &ateapi.ResumeActorResponse{Actor: item}, nil
}

func (f *fakeControl) DeleteActor(ctx context.Context, req *ateapi.DeleteActorRequest) (*ateapi.Actor, error) {
	if err := f.enter(ctx, "DeleteActor"); err != nil {
		return nil, err
	}
	item, err := f.actor("DeleteActor", req.GetActor())
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.actors, req.GetActor().GetAtespace()+"/"+req.GetActor().GetName())
	return item, nil
}

func (f *fakeControl) actor(rpc string, ref *ateapi.ObjectRef) (*ateapi.Actor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	item, ok := f.actors[ref.GetAtespace()+"/"+ref.GetName()]
	if !ok {
		return nil, status.Error(codes.NotFound, fmt.Sprintf("%s: actor %s not found", rpc, ref.GetName()))
	}
	return item, nil
}

func (f *fakeControl) GetActorEgressPolicy(ctx context.Context, req *ateapi.GetActorEgressPolicyRequest) (*ateapi.EgressPolicy, error) {
	if err := f.enter(ctx, "GetActorEgressPolicy"); err != nil {
		return nil, err
	}
	if req.GetName() != PolicyName {
		return nil, status.Error(codes.InvalidArgument, "egress policy name must be default")
	}
	return f.egressOf(ctx, req.GetActor())
}

func (f *fakeControl) CreateActorEgressPolicy(ctx context.Context, req *ateapi.CreateActorEgressPolicyRequest) (*ateapi.EgressPolicy, error) {
	if err := f.enter(ctx, "CreateActorEgressPolicy"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := req.GetActor().GetAtespace() + "/" + req.GetActor().GetName()
	if _, ok := f.egress[key]; ok {
		return nil, status.Error(codes.AlreadyExists, "egress policy already exists")
	}
	f.egress[key] = req.GetEgressPolicy()
	return req.GetEgressPolicy(), nil
}

func (f *fakeControl) UpdateActorEgressPolicy(ctx context.Context, req *ateapi.UpdateActorEgressPolicyRequest) (*ateapi.EgressPolicy, error) {
	if err := f.enter(ctx, "UpdateActorEgressPolicy"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := req.GetActor().GetAtespace() + "/" + req.GetActor().GetName()
	if _, ok := f.egress[key]; !ok {
		return nil, status.Error(codes.NotFound, "egress policy not found")
	}
	f.egress[key] = req.GetEgressPolicy()
	return req.GetEgressPolicy(), nil
}

func (f *fakeControl) DeleteActorEgressPolicy(ctx context.Context, req *ateapi.DeleteActorEgressPolicyRequest) (*ateapi.EgressPolicy, error) {
	if err := f.enter(ctx, "DeleteActorEgressPolicy"); err != nil {
		return nil, err
	}
	if req.GetName() != PolicyName {
		return nil, status.Error(codes.InvalidArgument, "egress policy name must be default")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := req.GetActor().GetAtespace() + "/" + req.GetActor().GetName()
	item, ok := f.egress[key]
	if !ok {
		return nil, status.Error(codes.NotFound, "egress policy not found")
	}
	delete(f.egress, key)
	return item, nil
}

func (f *fakeControl) egressOf(ctx context.Context, actor *ateapi.ObjectRef) (*ateapi.EgressPolicy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	item, ok := f.egress[actor.GetAtespace()+"/"+actor.GetName()]
	if !ok {
		return nil, status.Error(codes.NotFound, "egress policy not found")
	}
	return item, nil
}

func (f *fakeControl) ListActorTemplates(ctx context.Context, req *ateapi.ListActorTemplatesRequest) (*ateapi.ListActorTemplatesResponse, error) {
	if err := f.enter(ctx, "ListActorTemplates"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var items []*ateapi.ActorTemplate
	for _, key := range sortedKeys(f.templates) {
		if f.templates[key].GetMetadata().GetAtespace() == req.GetAtespace() {
			items = append(items, f.templates[key])
		}
	}
	page, next := pageOut(items, req.GetOptions(), f.page)
	return &ateapi.ListActorTemplatesResponse{ActorTemplates: page, NextPageToken: next}, nil
}

func (f *fakeControl) GetActorTemplate(ctx context.Context, req *ateapi.GetActorTemplateRequest) (*ateapi.ActorTemplate, error) {
	if err := f.enter(ctx, "GetActorTemplate"); err != nil {
		return nil, err
	}
	return f.templateOf(req.GetActorTemplate())
}

func (f *fakeControl) CreateActorTemplate(ctx context.Context, req *ateapi.CreateActorTemplateRequest) (*ateapi.ActorTemplate, error) {
	if err := f.enter(ctx, "CreateActorTemplate"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	item := req.GetActorTemplate()
	key := item.GetMetadata().GetAtespace() + "/" + item.GetMetadata().GetName()
	if _, ok := f.templates[key]; ok {
		return nil, status.Error(codes.AlreadyExists, "actor template already exists")
	}
	f.templates[key] = item
	return item, nil
}

func (f *fakeControl) DeleteActorTemplate(ctx context.Context, req *ateapi.DeleteActorTemplateRequest) (*ateapi.ActorTemplate, error) {
	if err := f.enter(ctx, "DeleteActorTemplate"); err != nil {
		return nil, err
	}
	item, err := f.templateOf(req.GetActorTemplate())
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.templates, req.GetActorTemplate().GetAtespace()+"/"+req.GetActorTemplate().GetName())
	return item, nil
}

func (f *fakeControl) templateOf(ref *ateapi.ObjectRef) (*ateapi.ActorTemplate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	item, ok := f.templates[ref.GetAtespace()+"/"+ref.GetName()]
	if !ok {
		return nil, status.Error(codes.NotFound, "actor template not found")
	}
	return item, nil
}

func (f *fakeControl) ListWorkers(ctx context.Context, req *ateapi.ListWorkersRequest) (*ateapi.ListWorkersResponse, error) {
	if err := f.enter(ctx, "ListWorkers"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var items []*ateapi.Worker
	for _, name := range sortedKeys(f.workers) {
		items = append(items, f.workers[name])
	}
	page, next := pageOut(items, req.GetOptions(), f.page)
	return &ateapi.ListWorkersResponse{Workers: page, NextPageToken: next}, nil
}

func (f *fakeControl) GetWorker(ctx context.Context, req *ateapi.GetWorkerRequest) (*ateapi.Worker, error) {
	if err := f.enter(ctx, "GetWorker"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	item, ok := f.workers[req.GetWorker().GetName()]
	if !ok {
		return nil, status.Error(codes.NotFound, "worker not found")
	}
	return item, nil
}

func (f *fakeControl) DrainWorker(ctx context.Context, req *ateapi.DrainWorkerRequest) (*ateapi.DrainWorkerResponse, error) {
	if err := f.enter(ctx, "DrainWorker"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	item, ok := f.workers[req.GetWorker().GetName()]
	if !ok {
		return nil, status.Error(codes.NotFound, "worker not found")
	}
	return &ateapi.DrainWorkerResponse{Worker: item}, nil
}

func (f *fakeControl) GetGlobalAccessPolicy(ctx context.Context, req *ateapi.GetGlobalAccessPolicyRequest) (*ateapi.AccessPolicy, error) {
	if err := f.enter(ctx, "GetGlobalAccessPolicy"); err != nil {
		return nil, err
	}
	if req.GetName() != PolicyName {
		return nil, status.Error(codes.InvalidArgument, "access policy name must be default")
	}
	return f.policyOf("")
}

func (f *fakeControl) CreateGlobalAccessPolicy(ctx context.Context, req *ateapi.CreateGlobalAccessPolicyRequest) (*ateapi.AccessPolicy, error) {
	if err := f.enter(ctx, "CreateGlobalAccessPolicy"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.policies[""]; ok {
		return nil, status.Error(codes.AlreadyExists, "global access policy already exists")
	}
	f.policies[""] = req.GetAccessPolicy()
	return req.GetAccessPolicy(), nil
}

func (f *fakeControl) UpdateGlobalAccessPolicy(ctx context.Context, req *ateapi.UpdateGlobalAccessPolicyRequest) (*ateapi.AccessPolicy, error) {
	if err := f.enter(ctx, "UpdateGlobalAccessPolicy"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.policies[""]; !ok {
		return nil, status.Error(codes.NotFound, "global access policy not found")
	}
	f.policies[""] = req.GetAccessPolicy()
	return req.GetAccessPolicy(), nil
}

func (f *fakeControl) GetAtespaceAccessPolicy(ctx context.Context, req *ateapi.GetAtespaceAccessPolicyRequest) (*ateapi.AccessPolicy, error) {
	if err := f.enter(ctx, "GetAtespaceAccessPolicy"); err != nil {
		return nil, err
	}
	if req.GetName() != PolicyName {
		return nil, status.Error(codes.InvalidArgument, "access policy name must be default")
	}
	return f.policyOf(req.GetAtespace().GetName())
}

func (f *fakeControl) CreateAtespaceAccessPolicy(ctx context.Context, req *ateapi.CreateAtespaceAccessPolicyRequest) (*ateapi.AccessPolicy, error) {
	if err := f.enter(ctx, "CreateAtespaceAccessPolicy"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	scope := req.GetAtespace().GetName()
	if _, ok := f.policies[scope]; ok {
		return nil, status.Error(codes.AlreadyExists, "atespace access policy already exists")
	}
	f.policies[scope] = req.GetAccessPolicy()
	return req.GetAccessPolicy(), nil
}

func (f *fakeControl) UpdateAtespaceAccessPolicy(ctx context.Context, req *ateapi.UpdateAtespaceAccessPolicyRequest) (*ateapi.AccessPolicy, error) {
	if err := f.enter(ctx, "UpdateAtespaceAccessPolicy"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	scope := req.GetAtespace().GetName()
	if _, ok := f.policies[scope]; !ok {
		return nil, status.Error(codes.NotFound, "atespace access policy not found")
	}
	f.policies[scope] = req.GetAccessPolicy()
	return req.GetAccessPolicy(), nil
}

func (f *fakeControl) DeleteAtespaceAccessPolicy(ctx context.Context, req *ateapi.DeleteAtespaceAccessPolicyRequest) (*ateapi.AccessPolicy, error) {
	if err := f.enter(ctx, "DeleteAtespaceAccessPolicy"); err != nil {
		return nil, err
	}
	if req.GetName() != PolicyName {
		return nil, status.Error(codes.InvalidArgument, "access policy name must be default")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	item, ok := f.policies[req.GetAtespace().GetName()]
	if !ok {
		return nil, status.Error(codes.NotFound, "atespace access policy not found")
	}
	delete(f.policies, req.GetAtespace().GetName())
	return item, nil
}

func (f *fakeControl) policyOf(scope string) (*ateapi.AccessPolicy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	item, ok := f.policies[scope]
	if !ok {
		return nil, status.Error(codes.NotFound, "access policy not found")
	}
	return item, nil
}

func sortedKeys[T any](store map[string]T) []string {
	keys := make([]string, 0, len(store))
	for key := range store {
		keys = append(keys, key)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// ─── обвязка проверок ──────────────────────────────────────────────────────

// selfSign выписывает самоподписанный сертификат на имя сервиса -- как корень
// ClusterTrustBundle, которым подписан ateapi.
func selfSign(t *testing.T, name string) (tls.Certificate, []byte) {
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

// writeFile кладёт файл во временный каталог проверки.
func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// startFake поднимает подделку Control на bufconn с TLS на Audience; endpoint
// отличен от стандартного, поэтому клиент обязан сверять имя Audience.
func startFake(t *testing.T, fake *fakeControl) (*Client, string) {
	t.Helper()
	tokenFile := writeFile(t, "token", "first\n")
	caFile := writeFile(t, "ca.pem", "")
	cert, caPEM := selfSign(t, Audience)
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	listener := bufconn.Listen(16 * 1024)
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}})))
	ateapi.RegisterControlServer(server, fake)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	client, err := New(tokenFile, caFile, "passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return listener.DialContext(ctx)
		}))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return client, tokenFile
}

// asAPI вынимает *APIError из отказа проверки.
func asAPI(t *testing.T, err error) *APIError {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	return apiErr
}

// ─── проверки ──────────────────────────────────────────────────────────────

func TestListPaginatesByNextPageToken(t *testing.T) {
	fake := newFake()
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		fake.atespaces[name] = &ateapi.Atespace{Metadata: meta(name, "")}
	}
	client, _ := startFake(t, fake)
	out, err := client.ListAtespaces(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var names []string
	for _, item := range out {
		names = append(names, item.GetMetadata().GetName())
	}
	want := []string{"a", "b", "c", "d", "e"}
	if len(names) != len(want) {
		t.Fatalf("got %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("got %v, want %v", names, want)
		}
	}
	// Три страницы: первая без токена, дальше токены прошлых ответов.
	if len(fake.listCalls) != 3 || fake.listCalls[0] != "" ||
		fake.listCalls[1] != "2" || fake.listCalls[2] != "4" {
		t.Fatalf("page tokens walked wrong: %v", fake.listCalls)
	}
}

func TestBearerTokenRereadPerCall(t *testing.T) {
	fake := newFake()
	fake.atespaces["a"] = &ateapi.Atespace{Metadata: meta("a", "")}
	client, tokenFile := startFake(t, fake)
	ctx := context.Background()
	if _, err := client.GetAtespace(ctx, "a"); err != nil {
		t.Fatalf("get: %v", err)
	}
	if err := os.WriteFile(tokenFile, []byte("second\n"), 0o600); err != nil {
		t.Fatalf("rotate token: %v", err)
	}
	if _, err := client.GetAtespace(ctx, "a"); err != nil {
		t.Fatalf("get after rotation: %v", err)
	}
	want := []string{"Bearer first", "Bearer second"}
	if len(fake.auths) != 2 || fake.auths[0] != want[0] || fake.auths[1] != want[1] {
		t.Fatalf("authorization headers %v, want %v", fake.auths, want)
	}
}

func TestEmptyTokenFileIsARefusal(t *testing.T) {
	fake := newFake()
	fake.atespaces["a"] = &ateapi.Atespace{Metadata: meta("a", "")}
	client, tokenFile := startFake(t, fake)
	if err := os.WriteFile(tokenFile, []byte("  \n"), 0o600); err != nil {
		t.Fatalf("blank token: %v", err)
	}
	_, err := client.GetAtespace(context.Background(), "a")
	apiErr := asAPI(t, err)
	if apiErr.Status != 403 {
		t.Fatalf("status %d, want 403 (UNAUTHENTICATED)", apiErr.Status)
	}
	if !strings.Contains(apiErr.Detail, "ATEAPI_TOKEN") {
		t.Fatalf("detail %q must name ATEAPI_TOKEN", apiErr.Detail)
	}
}

func TestGRPCCodeToHTTPMapping(t *testing.T) {
	cases := []struct {
		code   codes.Code
		status int
		detail string
	}{
		{codes.NotFound, 404, "atespace a not found"},
		{codes.AlreadyExists, 409, "atespace a already exists"},
		{codes.Aborted, 409, "the actor has been modified"},
		{codes.FailedPrecondition, 409, "the atespace is not empty"},
		{codes.InvalidArgument, 400, "bad atespace name"},
		{codes.PermissionDenied, 403, "not an owner"},
		{codes.Unauthenticated, 403, "no token"},
		{codes.Unavailable, 503, "ateapi is down"},
		{codes.Internal, 502, ""},
		{codes.ResourceExhausted, 502, ""},
	}
	for _, test := range cases {
		fake := newFake()
		fake.failures["GetAtespace"] = rpcFailure{code: test.code, msg: test.detail}
		client, _ := startFake(t, fake)
		_, err := client.GetAtespace(context.Background(), "a")
		apiErr := asAPI(t, err)
		if apiErr.Status != test.status {
			t.Errorf("%s: status %d, want %d", test.code, apiErr.Status, test.status)
		}
		if test.detail != "" && apiErr.Detail != test.detail {
			t.Errorf("%s: detail %q, want %q", test.code, apiErr.Detail, test.detail)
		}
	}
}

func TestNewNamesMissingSettings(t *testing.T) {
	_, err := New("", "", "")
	apiErr := asAPI(t, err)
	if apiErr.Status != 503 {
		t.Fatalf("status %d, want 503", apiErr.Status)
	}
	if !strings.Contains(apiErr.Detail, "ATEAPI_TOKEN") || !strings.Contains(apiErr.Detail, "ATEAPI_CA") {
		t.Fatalf("detail %q must name ATEAPI_TOKEN and ATEAPI_CA", apiErr.Detail)
	}
}

func TestNewRejectsBadCA(t *testing.T) {
	tokenFile := writeFile(t, "token", "t")
	caFile := writeFile(t, "ca.pem", "not a certificate")
	_, err := New(tokenFile, caFile, "")
	apiErr := asAPI(t, err)
	if apiErr.Status != 503 || !strings.Contains(apiErr.Detail, "ATEAPI_CA") {
		t.Fatalf("got %+v, want 503 naming ATEAPI_CA", apiErr)
	}
}

func TestUnmarshalRejectsUnknownFieldAndNonObject(t *testing.T) {
	err := Unmarshal([]byte(`{"metadata":{"name":"a","evil":true}}`), &ateapi.Atespace{})
	apiErr := asAPI(t, err)
	if apiErr.Status != 400 || !strings.Contains(apiErr.Detail, "unknown") {
		t.Fatalf("got %+v, want 400 with unknown-field reason", apiErr)
	}
	err = Unmarshal([]byte(`[1, 2]`), &ateapi.Atespace{})
	apiErr = asAPI(t, err)
	if apiErr.Status != 400 || !strings.Contains(apiErr.Detail, "must be a JSON object") {
		t.Fatalf("got %+v, want 400 with object reason", apiErr)
	}
	err = Unmarshal([]byte(`{"metadata":{"name":"a"}}`), &ateapi.Atespace{})
	if err != nil {
		t.Fatalf("valid body rejected: %v", err)
	}
}

func TestMarshalUnmarshalRoundTrip(t *testing.T) {
	source := &ateapi.Actor{Metadata: meta("claude", "brood"),
		ActorTemplate: &ateapi.ObjectRef{Atespace: "brood", Name: "tpl"}}
	data, err := Marshal(source)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	back := &ateapi.Actor{}
	if err := Unmarshal(data, back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !proto.Equal(source, back) {
		t.Fatal("round trip changed the message")
	}
}

func TestActorEgressTemplateWorkerPolicyMethods(t *testing.T) {
	fake := newFake()
	fake.actors["brood/claude"] = &ateapi.Actor{Metadata: meta("claude", "brood")}
	fake.templates["brood/tpl"] = &ateapi.ActorTemplate{Metadata: meta("tpl", "brood")}
	fake.workers["worker-1"] = &ateapi.Worker{Metadata: meta("worker-1", ""), WorkerPool: "pool"}
	fake.policies[""] = &ateapi.AccessPolicy{Metadata: meta("default", "")}
	fake.policies["brood"] = &ateapi.AccessPolicy{Metadata: meta("default", "brood")}
	client, _ := startFake(t, fake)
	ctx := context.Background()

	// Актор: create уже есть, update по предусловию, suspend/resume, delete.
	updated := &ateapi.Actor{Metadata: meta("claude", "brood")}
	saved, err := client.UpdateActor(ctx, updated)
	if err != nil || saved.GetMetadata().GetName() != "claude" {
		t.Fatalf("update actor: %v %v", saved, err)
	}
	suspended, err := client.SuspendActor(ctx, "brood", "claude")
	if err != nil || suspended.GetMetadata().GetName() != "claude" {
		t.Fatalf("suspend actor: %v %v", suspended, err)
	}
	resumed, err := client.ResumeActor(ctx, "brood", "claude")
	if err != nil || resumed.GetMetadata().GetName() != "claude" {
		t.Fatalf("resume actor: %v %v", resumed, err)
	}
	if err = client.DeleteActor(ctx, "brood", "claude"); err != nil {
		t.Fatalf("delete actor: %v", err)
	}
	if _, ok := fake.actors["brood/claude"]; ok {
		t.Fatal("actor not deleted")
	}

	// Egress-политика актора: get требует имени default.
	policy := &ateapi.EgressPolicy{Metadata: meta(PolicyName, "brood")}
	if _, err = client.GetActorEgressPolicy(ctx, "brood", "claude"); asAPI(t, err).Status != 404 {
		t.Fatalf("get missing egress: %v", err)
	}
	if _, err = client.CreateActorEgressPolicy(ctx, "brood", "claude", policy); err != nil {
		t.Fatalf("create egress: %v", err)
	}
	if _, err = client.GetActorEgressPolicy(ctx, "brood", "claude"); err != nil {
		t.Fatalf("get egress: %v", err)
	}
	if _, err = client.UpdateActorEgressPolicy(ctx, "brood", "claude", policy); err != nil {
		t.Fatalf("update egress: %v", err)
	}
	if err = client.DeleteActorEgressPolicy(ctx, "brood", "claude"); err != nil {
		t.Fatalf("delete egress: %v", err)
	}

	// Шаблоны: list по atespace, get, create, delete.
	if _, err = client.ListActorTemplates(ctx, "brood"); err != nil {
		t.Fatalf("list templates: %v", err)
	}
	if _, err = client.GetActorTemplate(ctx, "brood", "tpl"); err != nil {
		t.Fatalf("get template: %v", err)
	}
	fresh := &ateapi.ActorTemplate{Metadata: meta("new", "brood")}
	if _, err = client.CreateActorTemplate(ctx, fresh); err != nil {
		t.Fatalf("create template: %v", err)
	}
	if err = client.DeleteActorTemplate(ctx, "brood", "tpl"); err != nil {
		t.Fatalf("delete template: %v", err)
	}

	// Воркеры: list, get, drain.
	workers, err := client.ListWorkers(ctx)
	if err != nil || len(workers) != 1 {
		t.Fatalf("list workers: %v %v", workers, err)
	}
	if _, err = client.GetWorker(ctx, "worker-1"); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	drained, err := client.DrainWorker(ctx, "worker-1")
	if err != nil || drained.GetWorkerPool() != "pool" {
		t.Fatalf("drain worker: %v %v", drained, err)
	}

	// Политики доступа: имя default ставит клиент, не вызывающий.
	if _, err = client.GetGlobalAccessPolicy(ctx); err != nil {
		t.Fatalf("get global policy: %v", err)
	}
	if _, err = client.GetAtespaceAccessPolicy(ctx, "brood"); err != nil {
		t.Fatalf("get atespace policy: %v", err)
	}
	updated1 := &ateapi.AccessPolicy{Metadata: meta(PolicyName, "")}
	if _, err = client.UpdateGlobalAccessPolicy(ctx, updated1); err != nil {
		t.Fatalf("update global policy: %v", err)
	}
	if _, err = client.UpdateAtespaceAccessPolicy(ctx, "brood", updated1); err != nil {
		t.Fatalf("update atespace policy: %v", err)
	}
	if err = client.DeleteAtespaceAccessPolicy(ctx, "brood"); err != nil {
		t.Fatalf("delete atespace policy: %v", err)
	}

	want := []string{
		"UpdateActor", "SuspendActor", "ResumeActor", "DeleteActor",
		"GetActorEgressPolicy", "CreateActorEgressPolicy", "GetActorEgressPolicy",
		"UpdateActorEgressPolicy", "DeleteActorEgressPolicy",
		"ListActorTemplates", "GetActorTemplate", "CreateActorTemplate", "DeleteActorTemplate",
		"ListWorkers", "GetWorker", "DrainWorker",
		"GetGlobalAccessPolicy", "GetAtespaceAccessPolicy",
		"UpdateGlobalAccessPolicy", "UpdateAtespaceAccessPolicy", "DeleteAtespaceAccessPolicy",
	}
	if len(fake.calls) != len(want) {
		t.Fatalf("calls %v, want %v", fake.calls, want)
	}
	for i := range want {
		if fake.calls[i] != want[i] {
			t.Fatalf("calls %v, want %v", fake.calls, want)
		}
	}
}

func TestUpdateActorCarriesPreconditions(t *testing.T) {
	fake := newFake()
	actor := &ateapi.Actor{Metadata: meta("claude", "brood")}
	actor.Metadata.Version = 7
	fake.actors["brood/claude"] = actor
	client, _ := startFake(t, fake)
	stale := &ateapi.Actor{Metadata: meta("claude", "brood")}
	stale.Metadata.Version = 6
	_, err := client.UpdateActor(context.Background(), stale)
	apiErr := asAPI(t, err)
	if apiErr.Status != 409 || !strings.Contains(apiErr.Detail, "modified") {
		t.Fatalf("got %+v, want 409 with modified reason", apiErr)
	}
}
