package clientapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// SwarmRequestOption describes a graph outcome or actor choice label.
type SwarmRequestOption struct {
	Label   string `json:"label"`
	Outcome string `json:"outcome"`
}

// SwarmRequestActor identifies the asking actor, not a policy confirmation target.
type SwarmRequestActor struct {
	Name        string `json:"name"`
	Incarnation int64  `json:"incarnation"`
}

// SwarmRequest is an operator inbox item. A recorded policy response is not
// evidence that its held graph transition has been applied.
type SwarmRequest struct {
	ID            string               `json:"id"`
	SwarmID       string               `json:"swarmId"`
	Origin        string               `json:"origin"`
	Revision      int64                `json:"revision,omitempty"`
	Sequence      *int64               `json:"sequence,omitempty"`
	NodeID        string               `json:"nodeId,omitempty"`
	Actor         *SwarmRequestActor   `json:"actor,omitempty"`
	Kind          string               `json:"kind"`
	Question      string               `json:"question"`
	Context       string               `json:"context,omitempty"`
	Options       []SwarmRequestOption `json:"options"`
	State         string               `json:"state"`
	CreatedAt     string               `json:"createdAt"`
	AnsweredAt    *string              `json:"answeredAt,omitempty"`
	AnsweredBy    string               `json:"answeredBy,omitempty"`
	Outcome       *string              `json:"outcome,omitempty"`
	Response      *string              `json:"response,omitempty"`
	Delivery      string               `json:"delivery,omitempty"`
	DeliveryError string               `json:"deliveryError,omitempty"`
}

// SwarmRequestAnswer carries a graph outcome, actor text, or exact policy answer.
// Revision is explicit for policy and absent for legacy actor/node requests.
type SwarmRequestAnswer struct {
	Outcome  string `json:"outcome,omitempty"`
	Response string `json:"response,omitempty"`
	Revision int64  `json:"revision,omitempty"`
}

// SwarmRequests lists open inbox items, or retained items when all is true.
func (c *Client) SwarmRequests(ctx context.Context, swarm string, all bool) ([]SwarmRequest, error) {
	if swarm == "" || len(swarm) > 4096 {
		return nil, errors.New("a bounded swarm ID is required")
	}
	path := "/api/swarms/" + url.PathEscape(swarm) + "/requests"
	if all {
		path += "?all=1"
	}
	var result struct {
		Results []SwarmRequest `json:"results"`
	}
	if err := c.call(ctx, http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}
	return result.Results, nil
}

// AnswerSwarmRequest records one answer through the session/CSRF transport.
// It never infers a policy revision or retries with a newer one.
func (c *Client) AnswerSwarmRequest(ctx context.Context, swarm, request string, in SwarmRequestAnswer) (SwarmRequest, error) {
	if swarm == "" || request == "" || len(swarm) > 4096 || len(request) > 4096 || len(in.Response) > 16384 {
		return SwarmRequest{}, errors.New("bounded swarm, request and response are required")
	}
	if strings.HasPrefix(request, "policy:") {
		if in.Revision != 1 || in.Outcome != "" || in.Response != "yes" && in.Response != "no" {
			return SwarmRequest{}, errors.New("policy confirmations require revision 1 and a yes/no response")
		}
	} else if in.Revision != 0 {
		return SwarmRequest{}, errors.New("revision is only valid for policy confirmations")
	}
	body, err := json.Marshal(in)
	if err != nil {
		return SwarmRequest{}, errors.New("could not encode request answer")
	}
	var result SwarmRequest
	err = c.call(ctx, http.MethodPost, "/api/swarms/"+url.PathEscape(swarm)+"/requests/"+url.PathEscape(request)+"/answer", body, &result)
	if err != nil {
		return SwarmRequest{}, err
	}
	return result, nil
}
