package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"brood.dev/go/brood/workflowpolicy"
)

func (s *Store) readPassTraceRequests(ctx context.Context, trace *SwarmPassTrace, budget *passTraceBudget) ([]SwarmRequest, error) {
	last := trace.Steps[len(trace.Steps)-1].Visit.Sequence
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT `+swarmRequestCols+` FROM swarm_requests q
 WHERE q.swarm_id=? AND (q.sequence BETWEEN ? AND ? OR (q.origin='actor' AND (
 EXISTS(SELECT 1 FROM swarm_request_pass_contexts c WHERE c.request_id=q.id AND c.swarm_id=q.swarm_id AND c.pass_sequence=?)
 OR NOT EXISTS(SELECT 1 FROM swarm_request_pass_contexts c WHERE c.request_id=q.id AND c.swarm_id=q.swarm_id))))
 ORDER BY q.created_at,q.id LIMIT ?`, trace.SwarmID, trace.Pass, last, trace.Pass, MaxPassTraceRecords-budget.records+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []SwarmRequest{}
	for rows.Next() {
		request, err := scanSwarmRequest(rows)
		if err != nil {
			return nil, err
		}
		if err := budget.record(request); err != nil {
			return nil, err
		}
		if err := passTraceRequestTimeGap(trace, budget, request); err != nil {
			return nil, err
		}
		result = append(result, request)
	}
	return result, rows.Err()
}

func (s *Store) passTraceRequests(ctx context.Context, trace *SwarmPassTrace, sequences map[int64]bool, budget *passTraceBudget) error {
	requests, err := s.readPassTraceRequests(ctx, trace, budget)
	if err != nil {
		return err
	}
	for _, request := range requests {
		item := SwarmPassTraceRequest{Request: request}
		if request.Origin == "actor" {
			captured, err := s.GetSwarmRequestPassContext(ctx, trace.SwarmID, request.ID)
			if errors.Is(err, ErrNotFound) {
				gap := SwarmPassTraceGap{Kind: "actor_request_pass_not_retained", RequestID: request.ID}
				if err := budget.add(gap); err != nil {
					return err
				}
				trace.UnscopedActorRequests = append(trace.UnscopedActorRequests, request)
				trace.Gaps = append(trace.Gaps, gap)
				continue
			}
			if err != nil {
				return err
			}
			if captured.Pass != trace.Pass {
				continue
			}
			item.CreatedDuring = &captured
			item.Delivery, err = s.passTraceDelivery(ctx, trace.SwarmID, "answer", request.ID)
			if err != nil {
				return err
			}
		} else if request.Sequence == nil || !sequences[*request.Sequence] {
			continue
		}
		extra := item
		extra.Request = SwarmRequest{}
		if err := budget.add(extra); err != nil {
			return err
		}
		trace.Requests = append(trace.Requests, item)
	}
	for _, step := range trace.Steps {
		for _, candidate := range step.PolicyCandidates {
			if candidate.Preview.Decision.State != workflowpolicy.WaitingHuman || len(candidate.Preview.Decision.Human) == 0 {
				continue
			}
			request, err := s.pipelinePolicyRequest(ctx, trace.SwarmID, candidate.ID)
			if err != nil {
				return err
			}
			item := SwarmPassTraceRequest{Request: request}
			if err := budget.record(item); err != nil {
				return err
			}
			if err := passTraceRequestTimeGap(trace, budget, request); err != nil {
				return err
			}
			trace.Requests = append(trace.Requests, item)
		}
	}
	slices.SortFunc(trace.Requests, func(a, b SwarmPassTraceRequest) int {
		return comparePassTraceRequests(a.Request, b.Request)
	})
	slices.SortFunc(trace.UnscopedActorRequests, comparePassTraceRequests)
	return nil
}

func passTraceRequestTimeGap(trace *SwarmPassTrace, budget *passTraceBudget, request SwarmRequest) error {
	if _, err := time.Parse(time.RFC3339Nano, request.CreatedAt); err == nil {
		return nil
	}
	gap := SwarmPassTraceGap{Kind: "request_time_unavailable", RequestID: request.ID}
	if err := budget.add(gap); err != nil {
		return err
	}
	trace.Gaps = append(trace.Gaps, gap)
	return nil
}

func comparePassTraceRequests(a, b SwarmRequest) int {
	first, firstErr := time.Parse(time.RFC3339Nano, a.CreatedAt)
	second, secondErr := time.Parse(time.RFC3339Nano, b.CreatedAt)
	if firstErr == nil && secondErr == nil {
		if order := first.Compare(second); order != 0 {
			return order
		}
	} else if firstErr == nil {
		return -1
	} else if secondErr == nil {
		return 1
	}
	return strings.Compare(a.ID, b.ID)
}
