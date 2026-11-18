package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"brood.dev/go/brood/app"
)

type upgradeFaultWriter struct {
	http.ResponseWriter
	conn net.Conn
	rw   *bufio.ReadWriter
}

func (w upgradeFaultWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) { return w.conn, w.rw, nil }

type upgradeRejectWriter struct{ calls *atomic.Int32 }

func (w upgradeRejectWriter) Write([]byte) (int, error) {
	w.calls.Add(1)
	return 0, errors.New("client disconnected before 101")
}

type upgradeWriteFaultConn struct {
	net.Conn
	writes *atomic.Int32
}

func (c upgradeWriteFaultConn) Write([]byte) (int, error) {
	c.writes.Add(1)
	return 0, io.ErrClosedPipe
}

func TestUpgradeSubmissionBoundaryOutcomes(t *testing.T) {
	for _, fault := range []string{"dial", "guard refusal", "canceled after guard", "write"} {
		t.Run(fault, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var writes atomic.Int32
			checks, releases, finishes := 0, 0, 0
			outcome := UpgradeSubmissionUnknown
			p := New(Config{
				BeginUpgrade: func(*http.Request) (UpgradeReservation, error) {
					return UpgradeReservation{
						BeforeSubmit: func() (func(), error) {
							checks++
							if fault == "guard refusal" {
								return nil, errors.New("operation interrupted")
							}
							if fault == "canceled after guard" {
								cancel()
							}
							return func() { releases++ }, nil
						},
						Finish: func(value UpgradeOutcome) { finishes++; outcome = value },
					}, nil
				},
				DialContext: func(context.Context, string, string) (net.Conn, error) {
					if fault == "dial" {
						return nil, errors.New("unavailable")
					}
					a, b := net.Pipe()
					t.Cleanup(func() { b.Close() })
					return upgradeWriteFaultConn{a, &writes}, nil
				},
			})
			defer p.Close()
			a, b := net.Pipe()
			defer b.Close()
			w := upgradeFaultWriter{httptest.NewRecorder(), a, bufio.NewReadWriter(bufio.NewReader(strings.NewReader("")), bufio.NewWriter(io.Discard))}
			r := httptest.NewRequest("GET", "http://panel.invalid/ws", nil).WithContext(ctx)
			r.Header.Set("Upgrade", "websocket")
			p.serveUpgrade(w, r, Identity{}, app.Swarm{}, "a", &url.URL{Scheme: "http", Host: "actor.invalid"})
			wantChecks, wantReleases, wantWrites := 1, 1, int32(0)
			wantOutcome := UpgradeNotSubmitted
			if fault == "dial" {
				wantChecks, wantReleases = 0, 0
			}
			if fault == "guard refusal" {
				wantReleases = 0
			}
			if fault == "write" {
				wantWrites, wantOutcome = 1, UpgradeSubmissionUnknown
			}
			if checks != wantChecks || releases != wantReleases || writes.Load() != wantWrites || finishes != 1 || outcome != wantOutcome {
				t.Fatalf("checks=%d releases=%d writes=%d finishes=%d outcome=%v; want %d %d %d 1 %v", checks, releases, writes.Load(), finishes, outcome, wantChecks, wantReleases, wantWrites, wantOutcome)
			}
		})
	}
}

func TestUpgradeSubmissionSurvivesHandshakeFaults(t *testing.T) {
	for _, fault := range []string{"lost upstream response", "client 101 write", "effect then refusal"} {
		t.Run(fault, func(t *testing.T) {
			var effects atomic.Int32
			outcome := UpgradeNotSubmitted
			p := New(Config{BeginUpgrade: func(*http.Request) (UpgradeReservation, error) {
				return UpgradeReservation{Finish: func(value UpgradeOutcome) { outcome = value }}, nil
			}, DialContext: func(context.Context, string, string) (net.Conn, error) {
				a, b := net.Pipe()
				go func() {
					defer b.Close()
					if _, err := http.ReadRequest(bufio.NewReader(b)); err != nil {
						return
					}
					effects.Add(1)
					switch fault {
					case "client 101 write":
						io.WriteString(b, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
					case "effect then refusal":
						io.WriteString(b, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
					}
				}()
				return a, nil
			}})
			defer p.Close()
			a, b := net.Pipe()
			defer b.Close()
			var clientWrites atomic.Int32
			var output io.Writer = io.Discard
			if fault == "client 101 write" {
				output = upgradeRejectWriter{&clientWrites}
			}
			w := upgradeFaultWriter{httptest.NewRecorder(), a, bufio.NewReadWriter(bufio.NewReader(strings.NewReader("")), bufio.NewWriter(output))}
			r := httptest.NewRequest("GET", "http://panel.invalid/ws", nil)
			r.Header.Set("Upgrade", "websocket")
			p.serveUpgrade(w, r, Identity{}, app.Swarm{}, "a", &url.URL{Scheme: "http", Host: "actor.invalid"})
			if effects.Load() != 1 {
				t.Fatalf("fault did not reach upstream effect: %d", effects.Load())
			}
			if fault == "client 101 write" && clientWrites.Load() != 1 {
				t.Fatal("client 101 failure was not exercised")
			}
			if outcome != UpgradeSubmissionUnknown {
				t.Fatal("upstream effect occurred but cleanup reported no submission")
			}
		})
	}
}
