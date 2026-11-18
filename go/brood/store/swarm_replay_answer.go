package store

import (
	"errors"
	"fmt"
)

// ErrSwarmReplayMismatch means a graph question has no exact retained answer.
// An eval adapter must stop rather than fabricate an answer or ask live policy
// approval on behalf of the historical human.
var ErrSwarmReplayMismatch = errors.New("eval replay answer mismatch")

// SwarmReplayAnswer is selected historical data, not a human session, policy
// confirmation or execution permission. RecordedBy describes the source author
// only; it must never be impersonated when recording a new eval answer.
type SwarmReplayAnswer struct {
	ReferenceSwarmID   string
	ReferencePass      int64
	ReferenceSequence  int64
	ReferenceRequestID string
	NodeID             string
	Occurrence         int
	CandidateSwarmID   string
	CandidateSequence  int64
	CandidateRequestID string
	Outcome            string
	Response           *string
	RecordedBy         string
}

// MatchSwarmReplayAnswer selects a graph choice by node and one-based recorded
// visit occurrence. It is pure and repeatable: no answer is consumed or applied.
// Choice ordering may change, but the complete outcome/label set must match.
// Actor questions and policy confirmations never supply graph answers.
//
// Callers derive nodeID and occurrence from trusted graph history, separately
// authorize both scopes, pin the reference goal/commit, and enforce durable
// nonproduction eval isolation and policy bans before applying any answer. This
// function alone does none of that and must not be wired to live approvals.
func MatchSwarmReplayAnswer(reference SwarmPassTrace, nodeID string, occurrence int, question SwarmRequest) (SwarmReplayAnswer, error) {
	if !validComparisonTrace(reference) || !validPipelineField(nodeID) || occurrence < 1 || occurrence > MaxPassTraceRecords || len(reference.Requests)+len(reference.UnscopedActorRequests)+len(reference.Steps) > MaxPassTraceRecords {
		return SwarmReplayAnswer{}, replayMismatch("reference trace or occurrence is invalid")
	}
	if question.Origin != "node" || question.Kind != "choice" || question.State != "open" || question.ActorUID != "" || question.ActorIncarnation != 0 || question.Outcome != nil || question.Response != nil {
		return SwarmReplayAnswer{}, replayMismatch("only open graph choice requests can use recorded answers")
	}
	if !validPipelineField(question.ID) || !validPipelineField(question.SwarmID) || question.Sequence == nil || *question.Sequence < 1 || validRequestText(question.Question, true) != nil || validOptions(question.Options) != nil {
		return SwarmReplayAnswer{}, replayMismatch("candidate graph question is invalid")
	}
	sequences := map[int64]bool{}
	selected, seen := int64(0), 0
	for _, step := range reference.Steps {
		sequences[step.Visit.Sequence] = true
		if step.Visit.NodeID == nodeID {
			seen++
			if seen == occurrence {
				selected = step.Visit.Sequence
			}
		}
	}
	if selected == 0 {
		return SwarmReplayAnswer{}, replayMismatch("reference has no matching node visit")
	}
	var source *SwarmRequest
	ids, requests := map[string]bool{}, map[int64]bool{}
	for _, entry := range reference.Requests {
		request := entry.Request
		if request.Origin != "node" {
			continue
		}
		if !validPipelineField(request.ID) || request.SwarmID != reference.SwarmID || request.Sequence == nil || !sequences[*request.Sequence] || ids[request.ID] || requests[*request.Sequence] {
			return SwarmReplayAnswer{}, replayMismatch("reference graph questions are inconsistent or ambiguous")
		}
		ids[request.ID], requests[*request.Sequence] = true, true
		if *request.Sequence == selected {
			source = &request
		}
	}
	if source == nil {
		return SwarmReplayAnswer{}, replayMismatch("reference graph question is missing")
	}
	if source.Kind != "choice" || source.State != "answered" || source.Outcome == nil || *source.Outcome == "" {
		return SwarmReplayAnswer{}, replayMismatch("reference graph question has no recorded answer")
	}
	if validOptions(source.Options) != nil || validRequestText(source.Question, true) != nil {
		return SwarmReplayAnswer{}, replayMismatch("reference graph question choices are invalid")
	}
	if _, ok := source.OptionLabel(); !ok {
		return SwarmReplayAnswer{}, replayMismatch("recorded outcome is not a retained option")
	}
	if !sameReplayOptions(source.Options, question.Options) {
		return SwarmReplayAnswer{}, replayMismatch("answer options changed")
	}
	var response *string
	if source.Response != nil {
		if validRequestText(*source.Response, false) != nil {
			return SwarmReplayAnswer{}, replayMismatch("recorded response is invalid")
		}
		value := *source.Response
		response = &value
	}
	return SwarmReplayAnswer{ReferenceSwarmID: reference.SwarmID, ReferencePass: reference.Pass, ReferenceSequence: selected, ReferenceRequestID: source.ID, NodeID: nodeID, Occurrence: occurrence, CandidateSwarmID: question.SwarmID, CandidateSequence: *question.Sequence, CandidateRequestID: question.ID, Outcome: *source.Outcome, Response: response, RecordedBy: source.AnsweredBy}, nil
}

func sameReplayOptions(reference, candidate []RequestOption) bool {
	if len(reference) != len(candidate) {
		return false
	}
	labels := map[string]string{}
	for _, option := range reference {
		labels[option.Outcome] = option.Label
	}
	for _, option := range candidate {
		if label, ok := labels[option.Outcome]; !ok || label != option.Label {
			return false
		}
	}
	return true
}

func replayMismatch(reason string) error {
	return fmt.Errorf("%w: %s", ErrSwarmReplayMismatch, reason)
}
