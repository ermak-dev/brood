package server

// FEATURE: Роутер сообщений роя для managed-акторов (#115, эпик #113).
// SPEC: сообщение принимается только от авторизованного гостя и только
// актору того же роя; гостю-адресату запрос доезжает каналом brood-swarm-msg
// и вердикт возвращается отправителю; чужой рой и офлайн-адресат — текстовый
// отказ, не тишина.
import (
	"context"
	"errors"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

// dialRecorder подменяет доставку адресату: сервер должен открыть канал
// сообщений в гостя-получателя и вернуть его вердикт.
type dialRecorder struct {
	got chan relay.MessageRequest
}

func TestSwarmMessageRouterSameSwarmOnly(t *testing.T) {
	ts, alice, _, id, pve, hub := managedSetup(t)
	pve.startUPID = "UPID:node:00000001:00000002:00000003:vzstart:105:user@pve!brood:"
	pve.nextIDs = []int{105, 106}
	hub.connected = true
	deliveries := &dialRecorder{got: make(chan relay.MessageRequest, 4)}
	ts.srv.managed.deliverMessage = func(ctx context.Context, toUID string, toGeneration int64, req relay.MessageRequest) (relay.MessageResponse, error) {
		select {
		case deliveries.got <- req:
		default:
		}
		if req.To == "ghost" {
			return relay.MessageResponse{}, errors.New("recipient is not connected")
		}
		return relay.MessageResponse{OK: true, Verdict: "delivered"}, nil
	}
	ts.srv.managed.enableSwarmMessages()
	path := "/api/swarms/" + id + "/actors"
	// Два актора одного роя.
	r, b, _ := ts.do(alice, "POST", path, ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "sender"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}})
	expectStatus(t, "create sender", r, b, 201)
	senderUID := b["metadata"].(map[string]any)["uid"].(string)
	r, b, _ = ts.do(alice, "POST", path, ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "peer"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}})
	expectStatus(t, "create peer", r, b, 201)
	for range 4 {
		ts.srv.managed.reconcileOnce(context.Background())
	}
	router := ts.srv.managed.swarmMessageRouter()
	// Свой рой: доставка доходит до адресата.
	resp := router(senderUID, relay.MessageRequest{To: "peer", Body: "hello", From: "sender"})
	if !resp.OK || resp.Verdict != "delivered" {
		t.Fatalf("same-swarm verdict: %#v", resp)
	}
	select {
	case req := <-deliveries.got:
		if req.To != "peer" || req.From != "sender" || req.Body != "hello" {
			t.Fatalf("delivered request: %#v", req)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("delivery was not attempted")
	}
	// Офлайн-адресат — текстовый вердикт.
	resp = router(senderUID, relay.MessageRequest{To: "ghost", Body: "hi", From: "sender"})
	if resp.OK || resp.Verdict == "" {
		t.Fatalf("offline verdict: %#v", resp)
	}
	// Чужое имя — отказ без обращения к доставке.
	resp = router(senderUID, relay.MessageRequest{To: "nobody", Body: "hi", From: "sender"})
	if resp.OK {
		t.Fatalf("unknown recipient accepted: %#v", resp)
	}
	// Отправитель из другого роя не адресует этот рой: отдельно ниже.
}

func TestSwarmMessageRouterForeignSwarmRefused(t *testing.T) {
	ts, alice, _, id, pve, hub := managedSetup(t)
	pve.startUPID = "UPID:node:00000001:00000002:00000003:vzstart:105:user@pve!brood:"
	pve.nextIDs = []int{105, 106}
	hub.connected = true
	dialAttempts := make(chan struct{}, 4)
	ts.srv.managed.deliverMessage = func(context.Context, string, int64, relay.MessageRequest) (relay.MessageResponse, error) {
		select {
		case dialAttempts <- struct{}{}:
		default:
		}
		return relay.MessageResponse{OK: true, Verdict: "delivered"}, nil
	}
	ts.srv.managed.enableSwarmMessages()
	path := "/api/swarms/" + id + "/actors"
	r, b, _ := ts.do(alice, "POST", path, ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "sender"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}})
	expectStatus(t, "create sender", r, b, 201)
	senderUID := b["metadata"].(map[string]any)["uid"].(string)
	for range 4 {
		ts.srv.managed.reconcileOnce(context.Background())
	}
	// Второй рой того же проекта: адресат с тем же именем живёт в нём.
	sw, err := ts.srv.store.GetSwarm(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.srv.store.CreateSwarm(context.Background(), store.Swarm{ID: "swarm-two", ProjectID: sw.ProjectID, OwnerID: sw.OwnerID, Name: "beta", AtespaceName: "beta", AtespaceUID: strPtr("native-beta"), State: "active"}); err != nil {
		t.Fatal(err)
	}
	pools, err := ts.srv.store.ListManagedPools(context.Background())
	if err != nil || len(pools) == 0 {
		t.Fatalf("activated pool missing: %v", err)
	}
	peer := store.ManagedActor{UID: "uid-foreign", Name: "foreign", SwarmID: "swarm-two", AtespaceUID: "native-beta", PoolUID: pools[0].UID, Resources: store.ManagedResources{CPUMillicores: 1000, MemoryBytes: 1024, DiskBytes: 2048}, CTID: 107, OwnerNonce: "owner-foreign", OperationID: "create-foreign"}
	if _, err := ts.srv.store.ReserveManagedActor(context.Background(), peer); err != nil {
		t.Fatal(err)
	}
	router := ts.srv.managed.swarmMessageRouter()
	resp := router(senderUID, relay.MessageRequest{To: "foreign", Body: "hello", From: "sender"})
	if resp.OK {
		t.Fatalf("foreign swarm accepted: %#v", resp)
	}
	select {
	case <-dialAttempts:
		t.Fatal("delivery must not be attempted across swarms")
	default:
	}
}

func strPtr(v string) *string { return &v }
