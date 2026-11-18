package server

// BUG: Child holds deny owner browser control and mere views imply writable input.
// HYPOTHESIS: Automatic reservations and owner authority share the same denial gate.
// SOLUTION: Explicit writable selection journals intervention; readonly never mutates.
// RESULT: FIXED — owner frames flow, automatic child effects remain reserved.
// STATUS: FIXED

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
	"golang.org/x/net/websocket"
)

type browserBoundaryHub struct {
	guestRelay
	address string
	dials   atomic.Int32
}

func (h *browserBoundaryHub) Dial(ctx context.Context, _ string, _ int64) (net.Conn, error) {
	h.dials.Add(1)
	return (&net.Dialer{}).DialContext(ctx, "tcp", h.address)
}

func mintChildView(t *testing.T, d *dispatchServer, mode string) (string, int) {
	t.Helper()
	client := *d.alice
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	r, err := client.Get(d.http.URL + "/api/swarms/" + d.swarm + "/proxy/actors/" + d.worker.Name + "/?mode=" + mode)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	return r.Header.Get("Location"), r.StatusCode
}
func childViewWS(t *testing.T, d *dispatchServer, location string) (*websocket.Conn, error) {
	t.Helper()
	path, query, found := strings.Cut(location, "?")
	u := d.http.URL + path + "ws"
	if found {
		u += "?" + query
	}
	cfg, err := websocket.NewConfig("ws"+strings.TrimPrefix(u, "http"), d.http.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Protocol = []string{"tty"}
	return websocket.DialConfig(cfg)
}
func browserBoundaryFixture(t *testing.T) (*dispatchServer, *browserBoundaryHub, <-chan []byte) {
	t.Helper()
	d := newChildEngineServer(t)
	frames := make(chan []byte, 8)
	upstream := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
		defer ws.Close()
		for {
			var data []byte
			if websocket.Message.Receive(ws, &data) != nil {
				return
			}
			frames <- data
			if websocket.Message.Send(ws, append([]byte{'0'}, data...)) != nil {
				return
			}
		}
	}))
	t.Cleanup(upstream.Close)
	h := &browserBoundaryHub{guestRelay: d.srv.managed.hub, address: strings.TrimPrefix(upstream.URL, "http://")}
	d.srv.managed.hub = h
	return d, h, frames
}
func TestManagedViewRequiresExplicitWritableSelection(t *testing.T) {
	d, h, _ := browserBoundaryFixture(t)
	if _, err := d.srv.managed.createChild(context.Background(), d.master, childArgs(d, "view-only")); err != nil {
		t.Fatal(err)
	}
	d.srv.children.dispatch(context.Background())
	before := d.committed()
	location, status := mintChildView(t, d, "")
	if status != http.StatusOK || location != "" {
		t.Fatalf("unselected view must offer modes, not grant implicit writer: %d %s", status, location)
	}
	if h.dials.Load() != 0 || d.committed().State != before.State {
		t.Fatal("merely viewing mode choices interrupted execution")
	}
}

func TestChildWritableViewTicketBoundary(t *testing.T) {
	for _, stage := range []string{"new", "existing"} {
		t.Run(stage, func(t *testing.T) {
			d, h, frames := browserBoundaryFixture(t)
			var old string
			if stage == "existing" {
				var status int
				old, status = mintChildView(t, d, "rw")
				if status != 303 {
					t.Fatal(status)
				}
			}
			if _, err := d.srv.managed.createChild(context.Background(), d.master, childArgs(d, "view-child")); err != nil {
				t.Fatal(err)
			}
			d.srv.children.dispatch(context.Background())
			if stage == "new" {
				var status int
				old, status = mintChildView(t, d, "rw")
				if status != 303 {
					t.Fatalf("explicit owner writable selection denied: %d", status)
				}
			}
			if d.committed().State != "inflight" {
				t.Fatal("ticket page alone marked execution unknown")
			}
			ws, err := childViewWS(t, d, old)
			if err != nil {
				t.Fatalf("owner stream denied by child hold: %v", err)
			}
			defer ws.Close()
			ws.SetDeadline(time.Now().Add(5 * time.Second))
			if d.committed().State != "unconfirmed" {
				t.Fatal("writable boundary did not commit interruption before bytes")
			}
			if err := websocket.Message.Send(ws, []byte("0owner-input")); err != nil {
				t.Fatal(err)
			}
			select {
			case frame := <-frames:
				if string(frame) != "0owner-input" {
					t.Fatalf("owner bytes changed: %q", frame)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("owner input never reached PTY")
			}
			var reply []byte
			if err := websocket.Message.Receive(ws, &reply); err != nil {
				t.Fatal(err)
			}
			if h.dials.Load() != 1 {
				t.Fatalf("owner stream did not reach guest once: %d", h.dials.Load())
			}
		})
	}
}
func TestChildLiveBrowserLifetimeReservation(t *testing.T) {
	d, _, frames := browserBoundaryFixture(t)
	location, status := mintChildView(t, d, "rw")
	if status != 303 {
		t.Fatal(status)
	}
	ws, err := childViewWS(t, d, location)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	ws.SetDeadline(time.Now().Add(5 * time.Second))
	for _, payload := range [][]byte{[]byte(`{"AuthToken":"","columns":80,"rows":24}`), []byte("0before-child")} {
		if err = websocket.Message.Send(ws, payload); err != nil {
			t.Fatal(err)
		}
		if got := <-frames; string(got) != string(payload) {
			t.Fatal("ttyd payload changed")
		}
		var reply []byte
		if err = websocket.Message.Receive(ws, &reply); err != nil {
			t.Fatal(err)
		}
	}
	child, enqueueErr := d.srv.managed.createChild(context.Background(), d.master, childArgs(d, "live-browser"))
	d.srv.children.dispatch(context.Background())
	if enqueueErr == nil {
		t.Fatalf("live writable ttyd connection admitted child %s", child.ID)
	}
	if err = websocket.Message.Send(ws, []byte("0after-refused-child")); err != nil {
		t.Fatal(err)
	}
	if got := <-frames; string(got) != "0after-refused-child" {
		t.Fatal("unrelated connection was interrupted")
	}
	d.mu.Lock()
	deliveries := len(d.messages)
	d.mu.Unlock()
	if deliveries != 1 {
		t.Fatalf("child instruction reached guest: %d", deliveries-1)
	}
	ws.Close()
	deadline := time.Now().Add(3 * time.Second)
	var op store.ActorOperation
	for {
		op, err = d.srv.store.CurrentActorOperation(context.Background(), d.worker.UID)
		if err == nil && op.State == "unconfirmed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("disconnect invented completion: %+v %v", op, err)
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := d.srv.managed.createChild(context.Background(), d.master, childArgs(d, "disconnected-browser")); err == nil {
		t.Fatal("disconnect released uncertain ttyd effects")
	}
	release := coordinationInvocation("dispatch", map[string]any{"to": d.worker.Name, "action": "release", "request_id": op.OperationID})
	if _, err := d.srv.executeOperatorCoordinationCommand(context.Background(), d.master.DelegatorID, d.swarm, release); err == nil {
		t.Fatal("dispatch release erased uncertain owner input evidence")
	}
	location, status = mintChildView(t, d, "rw")
	if status != 303 {
		t.Fatalf("uncertainty denied new explicit owner selection: %d", status)
	}
	next, err := childViewWS(t, d, location)
	if err != nil {
		t.Fatalf("uncertainty denied new owner writable connection: %v", err)
	}
	next.Close()
	t.Log("live ttyd frames preserved; overlapping automatic child deliveries=0; owner reconnect remains available while uncertainty is retained")
}
func TestChildReadonlyBrowserCannotUpgrade(t *testing.T) {
	d, h, _ := browserBoundaryFixture(t)
	location, status := mintChildView(t, d, "ro")
	if status != 303 {
		t.Fatal(status)
	}
	if ws, err := childViewWS(t, d, strings.Split(location, "?")[0]+"?mode=rw"); err == nil {
		ws.Close()
		t.Fatal("readonly ticket upgraded without any child hold")
	}
	if _, err := d.srv.managed.createChild(context.Background(), d.master, childArgs(d, "readonly-child")); err != nil {
		t.Fatal(err)
	}
	d.srv.children.dispatch(context.Background())
	if _, status := mintChildView(t, d, "ro"); status != 303 {
		t.Fatalf("child blocked new readonly capability: %d", status)
	}
	calls := 0
	d.srv.managed.hub = commandSessionHub{guestRelay: h, call: func(_ context.Context, _ string, _ int64, req relay.SessionRequest) (relay.SessionResponse, error) {
		calls++
		var args struct{ Action, Mode string }
		json.Unmarshal(req.Arguments, &args)
		if args.Action == "open" && args.Mode != "ro" {
			t.Error("readonly ticket opened writer")
		}
		if args.Action != "open" && args.Action != "read" && args.Action != "close" {
			t.Errorf("readonly sent %s", args.Action)
		}
		return relay.SessionResponse{OK: true, Outcome: "complete", Text: `{"tid":"readonly","output":"child is working","mode":"ro"}`}, nil
	}}
	r, err := http.Get(d.http.URL + location)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 200 || !strings.Contains(string(b), "child is working") || r.Header.Get("X-Brood-Terminal-Mode") != "ro" {
		t.Fatalf("readonly inspection unavailable: %d %q", r.StatusCode, b)
	}
	if calls == 0 {
		t.Fatal("readonly positive did not inspect session")
	}
	for _, suffix := range []string{"", "?mode=rw", "?readonly=false"} {
		ws, err := childViewWS(t, d, strings.Split(location, "?")[0]+suffix)
		if ws != nil {
			ws.Close()
		}
		if err == nil {
			t.Fatalf("readonly capability upgraded with %q", suffix)
		}
	}
	if h.dials.Load() != 0 {
		t.Fatal("readonly inspection opened writable ttyd")
	}
}
