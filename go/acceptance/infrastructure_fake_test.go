// FEATURE: Exercise real executable adapters against a TLS gRPC infrastructure fake.
// SPEC: The fake replaces only ateapi infrastructure, never Brood auth or HTTP.
// This verifies active swarm plumbing, not a real Substrate actor or worker.
package acceptance

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"brood.dev/go/brood/substrate"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type infrastructureFake struct {
	ateapi.UnimplementedControlServer
	mu        sync.Mutex
	atespaces map[string]*ateapi.Atespace
	actorGets int
}

func (f *infrastructureFake) GetAtespace(_ context.Context, req *ateapi.GetAtespaceRequest) (*ateapi.Atespace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	item := f.atespaces[req.GetAtespace().GetName()]
	if item == nil {
		return nil, status.Error(codes.NotFound, "atespace not found")
	}
	return proto.Clone(item).(*ateapi.Atespace), nil
}

func (f *infrastructureFake) CreateAtespace(_ context.Context, req *ateapi.CreateAtespaceRequest) (*ateapi.Atespace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := req.GetAtespace().GetMetadata().GetName()
	if f.atespaces[name] != nil {
		return nil, status.Error(codes.AlreadyExists, "atespace already exists")
	}
	item := &ateapi.Atespace{Metadata: &ateapi.ResourceMetadata{Name: name, Uid: "infrastructure-fake-uid-" + name, Version: 1}}
	f.atespaces[name] = item
	return proto.Clone(item).(*ateapi.Atespace), nil
}

func (f *infrastructureFake) DeleteAtespace(_ context.Context, req *ateapi.DeleteAtespaceRequest) (*ateapi.Atespace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := req.GetAtespace().GetName()
	item := f.atespaces[name]
	if item == nil {
		return nil, status.Error(codes.NotFound, "atespace not found")
	}
	delete(f.atespaces, name)
	return proto.Clone(item).(*ateapi.Atespace), nil
}

func (f *infrastructureFake) ListActors(_ context.Context, _ *ateapi.ListActorsRequest) (*ateapi.ListActorsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actorGets++
	return &ateapi.ListActorsResponse{Actors: []*ateapi.Actor{}}, nil
}

func startInfrastructureFake(t *testing.T) (*infrastructureFake, []string) {
	t.Helper()
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "acceptance infrastructure fake"},
		DNSNames: []string{substrate.Audience}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage:    x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	caFile := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	const bearer = "acceptance-infrastructure-only-token"
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte(bearer), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &infrastructureFake{atespaces: make(map[string]*ateapi.Atespace)}
	srv := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}})),
		grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
			md, _ := metadata.FromIncomingContext(ctx)
			if auth := md.Get("authorization"); len(auth) != 1 || auth[0] != "Bearer "+bearer {
				return nil, status.Error(codes.Unauthenticated, "infrastructure fake requires bearer credentials")
			}
			return next(ctx, req)
		}),
	)
	ateapi.RegisterControlServer(srv, f)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	t.Cleanup(func() {
		srv.Stop()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("infrastructure fake serve: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("infrastructure fake did not stop")
		}
	})
	return f, []string{"ATEAPI_ENDPOINT=" + ln.Addr().String(), "ATEAPI_CA_FILE=" + caFile, "ATEAPI_TOKEN_FILE=" + tokenFile}
}

func testInfrastructureFake(t *testing.T, bin binaries) {
	fake, env := startInfrastructureFake(t)
	s := setUp(t, bin, env...)
	var row swarm
	decode(t, s.alice.expect("POST", "/api/projects/"+s.shared.ID+"/swarms", map[string]string{"name": "alice-active-infrastructure-fake"}, 201), &row)
	assertSwarm(t, row, s.shared.ID, "active")
	fake.mu.Lock()
	item := fake.atespaces[row.AtespaceName]
	fake.mu.Unlock()
	if item == nil || item.GetMetadata().GetUid() == "" {
		t.Fatal("active swarm was not actually provisioned over TLS gRPC")
	}
	s.bob.expect("GET", "/api/swarms/"+row.ID, nil, 404)
	s.bob.expect("GET", "/api/swarms/"+row.ID+"/actors", nil, 404)
	var actors struct{ Results []any }
	decode(t, s.alice.expect("GET", "/api/swarms/"+row.ID+"/actors", nil, 200), &actors)
	if actors.Results == nil || len(actors.Results) != 0 {
		t.Error("infrastructure fake must not claim a running actor")
	}
	fake.mu.Lock()
	calls := fake.actorGets
	fake.mu.Unlock()
	if calls != 1 {
		t.Errorf("actor-list RPC count %d, want one authorized request only", calls)
	}
	s.f.stop()
	s.f.start()
	var restarted swarm
	decode(t, s.alice.expect("GET", "/api/swarms/"+row.ID, nil, 200), &restarted)
	assertSwarm(t, restarted, s.shared.ID, "active")
	if restarted.ID != row.ID || restarted.OwnerID != row.OwnerID {
		t.Error("active swarm identity changed on executable restart")
	}
	// Существующая сессия немедленно теряет доступ к уже активному рою.
	s.admin.expect("DELETE", "/api/projects/"+s.shared.ID+"/members/alice", nil, 204)
	s.alice.expect("GET", "/api/swarms/"+row.ID, nil, 404)
	s.alice.expect("GET", "/api/swarms/"+row.ID+"/actors", nil, 404)
	s.admin.expect("GET", "/api/swarms/"+row.ID, nil, 200)
	// Удаляем только ресурс в нашей подделке, не в реальном Substrate.
	s.admin.expect("DELETE", "/api/swarms/"+row.ID, nil, 204)
	fake.mu.Lock()
	remaining := len(fake.atespaces)
	fake.mu.Unlock()
	if remaining != 0 {
		t.Error("swarm deletion did not remove the infrastructure fake atespace")
	}
	s.admin.expect("GET", "/api/swarms/"+row.ID, nil, 404)
}
