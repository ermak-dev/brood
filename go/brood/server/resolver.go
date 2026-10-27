package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"brood.dev/go/brood/proxy"
	"brood.dev/go/brood/substrate"
)

// SubstrateResolver routes existing actors through atenet-router, which also
// resumes suspended actors. Worker pod addresses are never ingress endpoints.
type SubstrateResolver struct {
	client *substrate.Client
	router string
}

// NewSubstrateResolver uses an explicitly configured router authority.
func NewSubstrateResolver(client *substrate.Client, router string) *SubstrateResolver {
	return &SubstrateResolver{client: client, router: router}
}

func (r *SubstrateResolver) ActorUpstream(atespace, actor string) (*url.URL, error) {
	if r.client == nil {
		return nil, &substrate.APIError{Status: 503, Detail: errAteapiNotConfiguredText}
	}
	if _, err := r.client.GetActor(context.Background(), atespace, actor); err != nil {
		var apiErr *substrate.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return nil, proxy.ErrUnknownActor
		}
		return nil, err
	}
	if r.router == "" {
		return nil, &substrate.APIError{Status: 503, Detail: "ATENET_ROUTER is required"}
	}
	u, err := url.Parse("https://" + r.router)
	if err != nil || u.Host != r.router || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, &substrate.APIError{Status: 503, Detail: "ATENET_ROUTER must be a router host:port"}
	}
	return u, nil
}
