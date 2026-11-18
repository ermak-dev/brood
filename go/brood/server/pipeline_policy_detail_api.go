package server

import (
	"net/http"
	"strings"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/store"
	"brood.dev/go/brood/workflowpolicy"
)

type policyEvidenceJSON struct {
	ID          string   `json:"id"`
	Known       bool     `json:"known"`
	Complete    bool     `json:"complete"`
	Artifact    string   `json:"artifact"`
	ProducerUID string   `json:"producerUid"`
	Paths       []string `json:"paths"`
}

type policyMatchJSON struct {
	RuleID string   `json:"ruleId"`
	Paths  []string `json:"paths"`
}

type policyDecisionJSON struct {
	State         string            `json:"state"`
	Matches       []policyMatchJSON `json:"matches"`
	Human         []string          `json:"human"`
	Review        []string          `json:"review"`
	Denied        []string          `json:"denied"`
	Unknown       []string          `json:"unknown"`
	NotApplicable []string          `json:"notApplicable"`
}

type policyCandidateJSON struct {
	ID             string                `json:"id"`
	Version        int                   `json:"version"`
	SwarmID        string                `json:"swarmId"`
	Sequence       int64                 `json:"sequence"`
	Pass           int64                 `json:"pass"`
	PolicyRevision int64                 `json:"policyRevision"`
	Boundary       string                `json:"boundary"`
	CreatedAt      string                `json:"createdAt"`
	Target         string                `json:"target"`
	Terminal       bool                  `json:"terminal,omitempty"`
	TargetKind     string                `json:"targetKind"`
	Initial        bool                  `json:"initial"`
	ActionsKnown   bool                  `json:"actionsKnown"`
	Actions        []string              `json:"actions"`
	Rules          []workflowpolicy.Rule `json:"rules"`
	Evidence       policyEvidenceJSON    `json:"evidence"`
	Decision       policyDecisionJSON    `json:"decision"`
}

func (s *Server) swarmRequestPolicyDetail(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.swarmRequestAccess(w, r, app.ActionViewSwarm); !ok {
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeDetail(w, http.StatusMethodNotAllowed, "policy candidate detail is read-only")
		return
	}
	id, ok := strings.CutPrefix(r.PathValue("request"), "policy:")
	if !ok || id == "" {
		writeDetail(w, http.StatusNotFound, "swarm request not found")
		return
	}
	candidate, err := s.store.GetPipelinePolicyCandidateByID(r.Context(), r.PathValue("id"), id)
	if err != nil {
		swarmRequestError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, policyCandidateView(candidate))
}

func policyCandidateView(candidate store.PipelinePolicyCandidate) policyCandidateJSON {
	preview := candidate.Preview
	evidence := preview.Input.Evidence
	decision := preview.Decision
	matches := make([]policyMatchJSON, 0, len(decision.Matches))
	for _, match := range decision.Matches {
		matches = append(matches, policyMatchJSON{RuleID: match.RuleID, Paths: append([]string{}, match.Paths...)})
	}
	return policyCandidateJSON{
		ID: candidate.ID, Version: candidate.Version, SwarmID: preview.SwarmID, Sequence: preview.Sequence,
		Pass: preview.Policy.Pass, PolicyRevision: preview.Policy.Revision, Boundary: candidate.Boundary, CreatedAt: candidate.CreatedAt,
		Target: preview.Transition.Next, Terminal: preview.Transition.Terminal, TargetKind: preview.Input.TargetKind, Initial: preview.Input.Initial,
		ActionsKnown: preview.Input.ActionsKnown, Actions: append([]string{}, preview.Input.Actions...), Rules: preview.Policy.Rules,
		Evidence: policyEvidenceJSON{ID: preview.EvidenceID, Known: evidence.Known, Complete: evidence.Complete, Artifact: evidence.Artifact, ProducerUID: evidence.ProducerUID, Paths: append([]string{}, evidence.Paths...)},
		Decision: policyDecisionJSON{State: decision.State, Matches: matches, Human: append([]string{}, decision.Human...), Review: append([]string{}, decision.Review...), Denied: append([]string{}, decision.Denied...), Unknown: append([]string{}, decision.Unknown...), NotApplicable: append([]string{}, decision.NotApplicable...)},
	}
}
