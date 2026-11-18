package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestChildDirectAuthenticatedWebSocketFenced(t *testing.T) {
	d, h, _ := browserBoundaryFixture(t)
	response, body, _ := d.do(d.alice, "GET", "/api/auth/session", "", nil)
	if response.StatusCode != 200 || body["authenticated"] != true {
		t.Fatal("positive session authentication failed")
	}
	if _, err := d.srv.managed.createChild(context.Background(), d.master, childArgs(d, "direct-websocket")); err != nil {
		t.Fatal(err)
	}
	d.srv.children.dispatch(context.Background())
	endpoint := d.http.URL + "/api/swarms/" + d.swarm + "/proxy/actors/" + d.worker.Name + "/ws?mode=rw"
	request, _ := http.NewRequest("GET", endpoint, nil)
	request.Header.Set("Origin", d.http.URL)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	request.Header.Set("X-CSRFToken", d.csrf(d.alice))
	response, err := d.alice.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusSwitchingProtocols || h.dials.Load() != 1 {
		t.Fatalf("explicit owner direct websocket denied: status=%d dials=%d", response.StatusCode, h.dials.Load())
	}
	if d.committed().State != "unconfirmed" {
		t.Fatal("direct owner stream did not retain child interruption")
	}
}

func TestBrowserFailedUpgradeRetainsUnknownSubmission(t *testing.T) {
	d, h, _ := browserBoundaryFixture(t)
	location, status := mintChildView(t, d, "rw")
	if status != 303 {
		t.Fatal(status)
	}
	request, _ := http.NewRequest("GET", d.http.URL+strings.Split(location, "?")[0]+"ws", nil)
	request.Header.Set("Origin", d.http.URL)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-WebSocket-Version", "13")
	// Отказ handshake не даёт контрактной гарантии отсутствия upstream effects.
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != 400 || h.dials.Load() != 1 {
		t.Fatalf("wrong handshake boundary: status=%d dials=%d", response.StatusCode, h.dials.Load())
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		op, err := d.srv.store.CurrentActorOperation(context.Background(), d.worker.UID)
		if err != nil {
			t.Fatalf("submitted request lost its reservation: %+v %v", op, err)
		}
		if op.State == "unconfirmed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("failed handshake did not retain uncertainty: %+v %v", op, err)
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := d.srv.managed.createChild(context.Background(), d.master, childArgs(d, "after-failed-upgrade")); err == nil {
		t.Fatal("failed handshake released unknown upstream effects into automatic allocation")
	}
}
