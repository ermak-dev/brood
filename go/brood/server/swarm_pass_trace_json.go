package server

import (
	"time"

	"brood.dev/go/brood/store"
)

type passTraceJSON struct {
	Version               int                    `json:"version"`
	SwarmID               string                 `json:"swarmId"`
	Pass                  int64                  `json:"pass"`
	Goal                  string                 `json:"goal"`
	Source                swarmSourceJSON        `json:"source"`
	Definition            swarmDefinitionJSON    `json:"definition"`
	Graph                 string                 `json:"graph"`
	Current               bool                   `json:"current"`
	Execution             string                 `json:"execution,omitempty"`
	Steps                 []passTraceStepJSON    `json:"steps"`
	Requests              []passTraceRequestJSON `json:"requests"`
	UnscopedActorRequests []passTraceRequestJSON `json:"unscopedActorRequests"`
	Gaps                  []passTraceGapJSON     `json:"gaps"`
}

type passTraceGapJSON struct {
	Kind      string `json:"kind"`
	Sequence  int64  `json:"sequence,omitempty"`
	RequestID string `json:"requestId,omitempty"`
}

type passTraceActorJSON struct {
	UID         string `json:"uid"`
	Incarnation int64  `json:"incarnation"`
	CurrentName string `json:"currentName,omitempty"`
}

type passTraceReportedJSON struct {
	RequestID        string `json:"requestId"`
	ActorUID         string `json:"actorUid"`
	ActorIncarnation int64  `json:"actorIncarnation"`
	Outcome          string `json:"outcome"`
	Report           string `json:"report"`
	CreatedAt        string `json:"createdAt"`
}

type passTraceObservationJSON struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	Detail   string `json:"detail,omitempty"`
	Revision int64  `json:"revision"`
}

type passTraceArtifactJSON struct {
	Commit      string   `json:"commit"`
	BaseCommit  string   `json:"baseCommit"`
	ProducerUID string   `json:"producerUid"`
	Incarnation int64    `json:"incarnation"`
	Paths       []string `json:"paths"`
}

type passTraceStepJSON struct {
	Sequence          int64                       `json:"sequence"`
	NodeID            string                      `json:"nodeId"`
	State             string                      `json:"state"`
	StartedAt         *string                     `json:"startedAt,omitempty"`
	FinishedAt        *string                     `json:"finishedAt,omitempty"`
	RequestID         string                      `json:"requestId,omitempty"`
	Actor             *passTraceActorJSON         `json:"actor,omitempty"`
	AttestedCommit    string                      `json:"attestedCommit,omitempty"`
	Outcome           *string                     `json:"outcome,omitempty"`
	Report            string                      `json:"report,omitempty"`
	Detail            string                      `json:"detail,omitempty"`
	Instruction       *passTraceDeliveryJSON      `json:"instruction,omitempty"`
	ChildInstructions []passTraceDeliveryJSON     `json:"childInstructions"`
	EphemeralJobs     []passTraceEphemeralJobJSON `json:"ephemeralJobs"`
	Children          *childHistoryGroupJSON      `json:"children,omitempty"`
	ReportedResult    *passTraceReportedJSON      `json:"reportedResult,omitempty"`
	ResultObservation *passTraceObservationJSON   `json:"resultObservation,omitempty"`
	ArtifactEvidence  *passTraceArtifactJSON      `json:"artifactEvidence,omitempty"`
	PolicyCandidates  []policyCandidateJSON       `json:"policyCandidates"`
}

type passTraceAttemptJSON struct {
	Number         int    `json:"number"`
	Mode           string `json:"mode"`
	CreatedAt      string `json:"createdAt"`
	DeadlineAt     string `json:"deadlineAt"`
	ResultRetained bool   `json:"resultRetained"`
	Outcome        string `json:"outcome,omitempty"`
	Detail         string `json:"detail,omitempty"`
	ObservedAt     string `json:"observedAt,omitempty"`
}

type passTraceDeliveryJSON struct {
	ID               string                 `json:"id"`
	SourceKind       string                 `json:"sourceKind"`
	SourceID         string                 `json:"sourceId"`
	ActorUID         string                 `json:"actorUid"`
	ActorIncarnation int64                  `json:"actorIncarnation"`
	CreatedAt        string                 `json:"createdAt"`
	ProbeOnly        bool                   `json:"probeOnly"`
	Frame            string                 `json:"frame"`
	Attempts         []passTraceAttemptJSON `json:"attempts"`
	SupervisorInbox  *passTraceInboxJSON    `json:"supervisorInbox,omitempty"`
}

type passTraceInboxJSON struct {
	ReceiverID    string `json:"receiverId"`
	PayloadDigest string `json:"payloadDigest"`
	Milestone     string `json:"milestone"`
}

type passTraceRequestContextJSON struct {
	Pass             int64 `json:"pass"`
	ObservedSequence int64 `json:"observedSequence"`
}

type passTraceRequestJSON struct {
	swarmRequestJSON
	ActorUID         string                       `json:"actorUid,omitempty"`
	ActorIncarnation int64                        `json:"actorIncarnation,omitempty"`
	AnsweredByUID    string                       `json:"answeredByUid,omitempty"`
	CreatedDuring    *passTraceRequestContextJSON `json:"createdDuring,omitempty"`
	DeliveryHistory  *passTraceDeliveryJSON       `json:"deliveryHistory,omitempty"`
}

func passTraceView(trace store.SwarmPassTrace) passTraceJSON {
	result := passTraceJSON{Version: trace.Version, SwarmID: trace.SwarmID, Pass: trace.Pass, Goal: trace.Goal,
		Source:     swarmSourceJSON{Origin: trace.Origin, Commit: trace.SourceCommit},
		Definition: swarmDefinitionJSON{Name: trace.DefinitionName, Version: trace.DefinitionVersion, Revision: trace.DefinitionRevision},
		Graph:      trace.Graph, Current: trace.Current, Execution: trace.CurrentExecution,
		Steps: []passTraceStepJSON{}, Requests: []passTraceRequestJSON{}, UnscopedActorRequests: []passTraceRequestJSON{}, Gaps: []passTraceGapJSON{}}
	nodes := map[int64]string{}
	for _, step := range trace.Steps {
		nodes[step.Visit.Sequence] = step.Visit.NodeID
		result.Steps = append(result.Steps, passTraceStepView(step))
	}
	for _, request := range trace.Requests {
		result.Requests = append(result.Requests, passTraceRequestView(request, nodes))
	}
	for _, request := range trace.UnscopedActorRequests {
		result.UnscopedActorRequests = append(result.UnscopedActorRequests, passTraceRequestView(store.SwarmPassTraceRequest{Request: request}, nodes))
	}
	for _, gap := range trace.Gaps {
		result.Gaps = append(result.Gaps, passTraceGapJSON{Kind: gap.Kind, Sequence: gap.Sequence, RequestID: gap.RequestID})
	}
	return result
}

func passTraceStepView(step store.SwarmPassTraceStep) passTraceStepJSON {
	visit := step.Visit
	result := passTraceStepJSON{Sequence: visit.Sequence, NodeID: visit.NodeID, State: visit.State,
		StartedAt: visit.StartedAt, FinishedAt: visit.FinishedAt, RequestID: visit.RequestID,
		AttestedCommit: visit.AttestedCommit, Outcome: visit.Outcome, Report: visit.Report, Detail: visit.Detail,
		Instruction: passTraceDeliveryView(step.Instruction), ChildInstructions: []passTraceDeliveryJSON{}, EphemeralJobs: []passTraceEphemeralJobJSON{}, PolicyCandidates: []policyCandidateJSON{}}
	for _, job := range step.EphemeralJobs {
		result.EphemeralJobs = append(result.EphemeralJobs, passTraceEphemeralJobView(job))
	}
	for i := range step.ChildInstructions {
		result.ChildInstructions = append(result.ChildInstructions, *passTraceDeliveryView(&step.ChildInstructions[i]))
	}
	if visit.ActorUID != "" {
		result.Actor = &passTraceActorJSON{UID: visit.ActorUID, Incarnation: visit.ActorIncarnation, CurrentName: visit.ActorName}
	}
	if step.Children != nil {
		group := childHistoryView(*step.Children)
		result.Children = &group
	}
	if step.Completion != nil {
		in := step.Completion
		result.ReportedResult = &passTraceReportedJSON{RequestID: in.RequestID, ActorUID: in.ActorUID, ActorIncarnation: in.Incarnation, Outcome: in.Outcome, Report: in.Report, CreatedAt: in.CreatedAt}
	}
	if step.ResultObservation != nil {
		in := step.ResultObservation
		result.ResultObservation = &passTraceObservationJSON{ID: in.ID, State: in.State, Detail: in.Detail, Revision: in.Revision}
	}
	if step.ResultEvidence != nil {
		in := step.ResultEvidence
		result.ArtifactEvidence = &passTraceArtifactJSON{Commit: in.Head, BaseCommit: in.Commit, ProducerUID: in.ActorUID, Incarnation: in.Generation, Paths: append([]string{}, in.Paths...)}
	}
	for _, candidate := range step.PolicyCandidates {
		result.PolicyCandidates = append(result.PolicyCandidates, policyCandidateView(candidate))
	}
	return result
}

func passTraceDeliveryView(delivery *store.SwarmPassTraceDelivery) *passTraceDeliveryJSON {
	if delivery == nil {
		return nil
	}
	intent := delivery.Intent
	result := &passTraceDeliveryJSON{ID: intent.ID, SourceKind: intent.Input.SourceKind, SourceID: intent.Input.SourceID,
		ActorUID: intent.Input.ActorUID, ActorIncarnation: intent.Input.Incarnation, CreatedAt: intent.CreatedAt,
		ProbeOnly: intent.ProbeOnly, Frame: intent.Input.Payload, Attempts: []passTraceAttemptJSON{}}
	if inbox := delivery.SupervisorInbox; inbox != nil {
		milestone := "selected"
		if inbox.DispatchReserved {
			milestone = "dispatch_permit_reserved"
		} else if inbox.Custody {
			milestone = store.ActorDeliveryInboxAdmitted
		}
		result.SupervisorInbox = &passTraceInboxJSON{ReceiverID: inbox.Target.ReceiverID, PayloadDigest: inbox.Target.Digest, Milestone: milestone}
	}
	for _, observation := range delivery.Attempts {
		attempt := observation.Attempt
		item := passTraceAttemptJSON{Number: attempt.Number, Mode: attempt.Mode,
			CreatedAt: attempt.CreatedAt.UTC().Format(time.RFC3339Nano), DeadlineAt: attempt.DeadlineAt.UTC().Format(time.RFC3339Nano),
			ResultRetained: observation.Outcome != "", Outcome: observation.Outcome, Detail: observation.Detail}
		if !observation.ObservedAt.IsZero() {
			item.ObservedAt = observation.ObservedAt.UTC().Format(time.RFC3339Nano)
		}
		result.Attempts = append(result.Attempts, item)
	}
	return result
}

func passTraceRequestView(item store.SwarmPassTraceRequest, nodes map[int64]string) passTraceRequestJSON {
	request := item.Request
	base := swarmRequestBaseView(request)
	if request.Sequence != nil {
		base.NodeID = nodes[*request.Sequence]
	}
	result := passTraceRequestJSON{swarmRequestJSON: base, ActorUID: request.ActorUID, ActorIncarnation: request.ActorIncarnation,
		AnsweredByUID: request.AnsweredBy, DeliveryHistory: passTraceDeliveryView(item.Delivery)}
	if item.CreatedDuring != nil {
		result.CreatedDuring = &passTraceRequestContextJSON{Pass: item.CreatedDuring.Pass, ObservedSequence: item.CreatedDuring.ObservedSequence}
	}
	return result
}
