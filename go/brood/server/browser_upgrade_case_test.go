package server

// BUG: Reservation adapter не должна менять HTTP Upgrade token case semantics.
// HYPOTHESIS: Новый gate сравнивает websocket с учётом регистра.
// SOLUTION: Сравнение HTTP Upgrade token без учёта регистра.
// RESULT: FIXED — валидный WebSocket открывает прежний byte bridge под reservation.
// STATUS: FIXED

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestBrowserUpgradePreservesTokenCase(t *testing.T) {
	d, _, _ := browserBoundaryFixture(t)
	location, status := mintChildView(t, d, "rw")
	if status != 303 {
		t.Fatal(status)
	}
	request, _ := http.NewRequest("GET", d.http.URL+strings.Split(location, "?")[0]+"ws", nil)
	request.Header.Set("Origin", d.http.URL)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "WebSocket")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 101 {
		t.Fatalf("HTTP Upgrade token is case insensitive: %d", response.StatusCode)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		op, err := d.srv.store.CurrentActorOperation(context.Background(), d.worker.UID)
		if err == nil && op.State == "unconfirmed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stream settlement missing: %+v %v", op, err)
		}
		time.Sleep(time.Millisecond)
	}
}
