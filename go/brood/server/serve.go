// Запуск сервера: HTTP-слушатель, graceful shutdown по отмене контекста
// (SIGTERM/SIGINT): слушатель закрывается с ожиданием активных запросов,
// затем — база и каналы.
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// shutdownTimeout — сколько ждать незавершённые запросы при остановке.
const shutdownTimeout = 10 * time.Second

// Serve собирает сервер по cfg и слушает до отмены ctx или отказа
// слушателя.
func Serve(ctx context.Context, cfg Config) error {
	return serve(ctx, cfg, nil)
}

// ServeLog preserves the earlier entry point. Server libraries remain silent;
// readiness uses a context-aware channel rather than an arbitrary writer.
func ServeLog(ctx context.Context, cfg Config, _ io.Writer) error {
	return Serve(ctx, cfg)
}

func serve(ctx context.Context, cfg Config, ready chan<- net.Addr) error {
	srv, err := New(ctx, cfg, Options{})
	if err != nil {
		return err
	}
	defer srv.Close()

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(slog.Default().Handler(), slog.LevelError),
	}

	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.Addr, err)
	}
	defer listener.Close()
	errCh := make(chan error, 1)
	go func() {
		var err error
		if cfg.TLSCert != "" {
			err = httpSrv.ServeTLS(listener, cfg.TLSCert, cfg.TLSKey)
		} else {
			err = httpSrv.Serve(listener)
		}
		if !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()
	if ready != nil {
		select {
		case ready <- listener.Addr():
		case <-ctx.Done():
		}
	}

	// Остановка: отмена контекста или отказ слушателя — раньше того,
	// кто случится первым.
	listenCtx, listenCancel := context.WithCancel(ctx)
	defer listenCancel()
	select {
	case <-listenCtx.Done():
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("listen %s: %w", cfg.Addr, err)
		}
		// Слушатель закрылся сам (например, пустой адрес) — выходим.
		return nil
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	// Забираем результат горутины слушателя, чтобы не оставить её жить.
	<-errCh
	return nil
}
