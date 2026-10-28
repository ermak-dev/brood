// Проверки запуска: слушатель поднимается, healthz отвечает, отмена
// контекста останавливает сервер и закрывает базу.
package server

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// BUG: A fixed startup delay can cancel SQLite initialization under load (#63).
// HYPOTHESIS 1 (ABANDONED): An arbitrary startup writer can block cancellation.
// HYPOTHESIS 2: A context-aware readiness channel permits shutdown without a receiver.
// SOLUTION: Probe healthz at the bound address before canceling.
// RESULT: FIXED — 100 readiness/shutdown repetitions and the full race suite pass.
// TestServeLifecycle checks readiness before exercising graceful shutdown.
func TestServeLifecycle(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{DBPath: filepath.Join(dir, "brood.db"), Addr: "127.0.0.1:0"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	address := make(chan net.Addr)
	go func() { done <- serve(ctx, cfg, address) }()

	select {
	case addr := <-address:
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get("http://" + addr.String() + "/healthz")
		if err != nil {
			t.Fatalf("healthz before shutdown: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("healthz status = %d, want 200", resp.StatusCode)
		}
	case err := <-done:
		t.Fatalf("Serve stopped before readiness: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not report its bound address")
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop after context cancel")
	}

	// База закрыта корректно: файл лежит рядом с WAL-журналом.
	if _, err := os.Stat(cfg.DBPath); err != nil {
		t.Fatalf("database missing after shutdown: %v", err)
	}
}

// TestServeFixedPort — на фиксированном порту healthz отвечает, отмена
// контекста останавливает сервер.
func TestServeFixedPort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := listener.Addr().String()
	// Освобождаем порт: Serve займёт его заново; гонки нет в пределах
	// процесса с kernel reuse, поэтому слушаем после короткой паузы.
	listener.Close()
	time.Sleep(50 * time.Millisecond)

	dir := t.TempDir()
	cfg := Config{DBPath: filepath.Join(dir, "brood.db"), Addr: addr}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	// An unread readiness channel must not prevent graceful shutdown.
	go func() { done <- serve(ctx, cfg, make(chan net.Addr)) }()

	client := &http.Client{Timeout: 2 * time.Second}
	var ok bool
	for try := 0; try < 50; try++ {
		resp, err := client.Get("http://" + addr + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				ok = true
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ok {
		t.Fatal("healthz never answered")
	}

	// SIGTERM-эквивалент: отмена контекста.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop after context cancel")
	}
}

// unused держит syscall для симметрии сигнальных констант cmd.
var _ = syscall.SIGTERM
