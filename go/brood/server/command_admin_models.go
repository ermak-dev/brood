package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"brood.dev/go/brood/command"
)

// adminModelClient retains only the installed proxy's model-list capability.
// The parent server initializes its adminModels field with newAdminModelClient(cfg).
// It deliberately does not use the management key or guest-facing HTTP handlers.
type adminModelClient struct {
	client *http.Client
	origin string
	key    string
}

func newAdminModelClient(cfg Config) *adminModelClient {
	if cfg.ActorLLMURL != "" {
		cfg.CLIProxyURL, cfg.CLIProxyAPIKey, cfg.CLIProxyCAPEM = cfg.ActorLLMURL, cfg.ActorLLMAPIKey, cfg.ActorLLMCAPEM
	}
	if cfg.CLIProxyURL == "" || cfg.CLIProxyAPIKey == "" {
		return nil
	}
	transport := cliProxyTransport(cfg)
	transport.Proxy = nil
	transport.ResponseHeaderTimeout = 5 * time.Second
	transport.MaxResponseHeaderBytes = 64 << 10
	transport.DisableKeepAlives = true
	return &adminModelClient{origin: strings.TrimSuffix(cfg.CLIProxyURL, "/"), key: cfg.CLIProxyAPIKey, client: &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (c *adminModelClient) models(ctx context.Context) (command.Result, error) {
	if c == nil {
		return command.Result{}, fmt.Errorf("installed model proxy is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.origin+"/v1/models", nil)
	if err != nil {
		return command.Result{}, fmt.Errorf("model discovery unavailable")
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	resp, err := c.client.Do(req)
	if err != nil {
		return command.Result{}, fmt.Errorf("model discovery failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return command.Result{}, fmt.Errorf("model discovery failed")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return command.Result{}, fmt.Errorf("model discovery response exceeds limits")
	}
	var response struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &response); err != nil || response.Data == nil || len(response.Data) > 1000 {
		return command.Result{}, fmt.Errorf("invalid model discovery response")
	}
	models := []map[string]string{}
	for _, row := range response.Data {
		if row.ID == "" || len(row.ID) > 200 || strings.Contains(row.ID, c.key) {
			return command.Result{}, fmt.Errorf("invalid model discovery response")
		}
		for _, r := range row.ID {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
				return command.Result{}, fmt.Errorf("invalid model discovery response")
			}
		}
		models = append(models, map[string]string{"id": row.ID})
	}
	return adminCommandResult("Models advertised by the installed proxy; availability is not an inference health check.", map[string]any{"models": models})
}
