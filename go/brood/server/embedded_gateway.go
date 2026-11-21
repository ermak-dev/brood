package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	gatewayapi "github.com/router-for-me/CLIProxyAPI/v8/sdk/api"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy"
	gatewayconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	_ "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator/builtin"
	"golang.org/x/crypto/bcrypt"
)

const gatewayConfigSetting = "BROOD_CLIPROXY_CONFIG"

func gatewayConfigError(detail string) error {
	return &ConfigError{Setting: gatewayConfigSetting, Detail: detail}
}

type embeddedGateway struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func startEmbeddedGateway(ctx context.Context, cfg *Config) (*embeddedGateway, error) {
	data, err := os.ReadFile(cfg.CLIProxyConfig)
	if err != nil {
		return nil, gatewayConfigError("could not read the CLIProxyAPI YAML file")
	}
	upstream, err := gatewayconfig.ParseConfigBytes(data)
	if err != nil {
		return nil, gatewayConfigError("invalid CLIProxyAPI configuration")
	}
	if upstream.Host != "127.0.0.1" || upstream.Port < 1024 || upstream.Port > 65535 || upstream.TLS.Enable {
		return nil, gatewayConfigError("set server.host to 127.0.0.1 and server.port to an unprivileged port, and disable gateway TLS")
	}
	if !filepath.IsAbs(upstream.AuthDir) {
		return nil, gatewayConfigError("set oauth.auth-dir to an absolute path in persistent storage")
	}
	if !slices.Contains(upstream.APIKeys, cfg.CLIProxyAPIKey) {
		return nil, gatewayConfigError("access.api-keys must include the value configured in CLIPROXY_API_KEY")
	}
	if cfg.CLIProxyManagementKey != "" && bcrypt.CompareHashAndPassword([]byte(upstream.RemoteManagement.SecretKey), []byte(cfg.CLIProxyManagementKey)) != nil {
		return nil, gatewayConfigError("management.secret-key must match CLIPROXY_MANAGEMENT_KEY")
	}
	// Клиент не примет за шлюз другой процесс, уже занявший настроенный порт.
	var nonce [16]byte
	rand.Read(nonce[:])
	probe := "/_brood_ready/" + hex.EncodeToString(nonce[:])
	builder := cliproxy.NewBuilder().WithConfig(upstream).WithConfigPath(cfg.CLIProxyConfig).
		WithServerOptions(gatewayapi.WithEngineConfigurator(func(engine *gin.Engine) {
			engine.GET(probe, func(c *gin.Context) { c.Status(http.StatusNoContent) })
		}))
	service, err := builder.Build()
	if err != nil {
		return nil, gatewayConfigError("could not initialize the embedded CLIProxyAPI service")
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	gateway := &embeddedGateway{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(gateway.done)
		_ = service.Run(runCtx)
	}()

	origin := "http://" + net.JoinHostPort(upstream.Host, strconv.Itoa(upstream.Port))
	if err := gateway.wait(ctx, origin+probe); err != nil {
		gateway.close()
		return nil, err
	}
	cfg.CLIProxyURL = origin
	return gateway, nil
}

func (g *embeddedGateway) wait(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	transport := &http.Transport{DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-g.done:
			return gatewayConfigError("embedded CLIProxyAPI stopped during startup; check its configuration and local port")
		case <-ctx.Done():
			if ctx.Err() == context.Canceled {
				return ctx.Err()
			}
			return gatewayConfigError("embedded CLIProxyAPI did not become ready; check its configuration and local port")
		case <-ticker.C:
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return err
			}
			resp, err := client.Do(req)
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusNoContent {
					return nil
				}
			}
		}
	}
}

func (g *embeddedGateway) close() {
	g.cancel()
	timer := time.NewTimer(32 * time.Second)
	defer timer.Stop()
	select {
	case <-g.done:
	case <-timer.C:
	}
}
