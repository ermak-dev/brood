package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"time"

	"brood.dev/go/brood/actormodel"
)

func actorModelBackend(cfg Config) Config {
	if cfg.ActorLLMURL != "" {
		cfg.CLIProxyURL, cfg.CLIProxyAPIKey, cfg.CLIProxyCAPEM = cfg.ActorLLMURL, cfg.ActorLLMAPIKey, cfg.ActorLLMCAPEM
	}
	return cfg
}

func newRoleModelCatalog(cfg Config) func(context.Context) ([]string, error) {
	cfg = actorModelBackend(cfg)
	target, _ := url.Parse(cfg.CLIProxyURL)
	transport := cliProxyTransport(cfg)
	transport.Proxy = nil
	transport.MaxResponseHeaderBytes = 1 << 20
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return func(ctx context.Context) ([]string, error) {
		if target == nil || target.Host == "" || cfg.CLIProxyAPIKey == "" {
			return nil, errors.New("agent model backend is not configured")
		}
		endpoint := *target
		endpoint.Path = "/v1/models"
		ids := map[string]bool{}
		ok := false
		// Шлюз публикует разные списки для OpenAI и Anthropic; в первом есть GLM.
		for _, anthropic := range []bool{false, true} {
			if readRoleModelCatalog(ctx, client, endpoint, cfg.CLIProxyAPIKey, anthropic, ids) == nil {
				ok = true
			}
		}
		if !ok {
			return nil, errors.New("could not read the agent model catalog")
		}
		names := make([]string, 0, len(ids))
		for id := range ids {
			names = append(names, id)
		}
		slices.Sort(names)
		return names, nil
	}
}

func readRoleModelCatalog(ctx context.Context, client *http.Client, endpoint url.URL, key string, anthropic bool, ids map[string]bool) error {
	query := endpoint.Query()
	if anthropic {
		query.Set("limit", "1000")
	}
	for page := 0; page < 10; page++ {
		endpoint.RawQuery = query.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Accept", "application/json")
		if anthropic {
			req.Header.Set("Anthropic-Version", "2023-06-01")
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		const maxCatalog = 2 << 20
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalog+1))
		_ = resp.Body.Close()
		var payload struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
			HasMore bool   `json:"has_more"`
			LastID  string `json:"last_id"`
		}
		if err != nil || resp.StatusCode != http.StatusOK || len(raw) > maxCatalog || json.Unmarshal(raw, &payload) != nil || payload.Data == nil {
			return errors.New("invalid model catalog response")
		}
		for _, m := range payload.Data {
			if actormodel.ValidName(m.ID) {
				ids[m.ID] = true
			}
		}
		if !anthropic || !payload.HasMore {
			return nil
		}
		if payload.LastID == "" || payload.LastID == query.Get("after_id") {
			break
		}
		query.Set("after_id", payload.LastID)
	}
	return errors.New("model catalog pagination did not finish")
}

func (s *Server) handleRoleModels(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	if !s.freshAdmin(r.Context(), user.ID) {
		writeDetail(w, 403, "Only an administrator can list models for agent roles")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	models, err := s.roleModelCatalog(r.Context())
	if err != nil {
		writeDetail(w, 503, "Agent models are unavailable; check the model gateway and retry")
		return
	}
	writeJSON(w, 200, models)
}
