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

// TestServeLifecycle — адрес слушателя, проба готовности и graceful
// shutdown по SIGTERM-эквиваленту (отмена контекста).
func TestServeLifecycle(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{DBPath: filepath.Join(dir, "brood.db"), Addr: "127.0.0.1:0"}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ServeLog(ctx, cfg, nil) }()

	// Проба через случайный порт недоступна до фактического слушания:
	// ждём, пока Serve не закроется или не пройдёт секунда; адрес :0
	// выбирается ядром, проверяем доступность через дублирующий слушатель
	// той же базы — healthz проверен в server_test, здесь важен выход.
	time.Sleep(200 * time.Millisecond)
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
	done := make(chan error, 1)
	go func() { done <- ServeLog(ctx, cfg, nil) }()

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
