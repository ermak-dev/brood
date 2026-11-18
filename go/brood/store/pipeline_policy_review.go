package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"brood.dev/go/brood/pipeline"
	"brood.dev/go/brood/workflowpolicy"
)

var errPolicyReviewUnavailable = errors.New("independent review unavailable")

// PipelinePolicyReviewRequirement pins a configured review role and original work
// author to one immutable artifact. A prospective next review may satisfy a later rule.
type PipelinePolicyReviewRequirement struct{ SwarmID, CandidateID, RuleID, Artifact, ProducerUID, Role string }

// PipelinePolicyReviewAssignment binds only the next inline graph review visit.
// Neither a separate pass nor a caller-selected actor can supply review authority.
type PipelinePolicyReviewAssignment struct {
	CandidateID, RuleID, ReviewSwarmID, ActorUID string
	ReviewSequence, Incarnation                  int64
}

// PipelinePolicyReviewRequirements reads immutable obligations, never dispatch
// permission. Callers authorize material visibility before reading this history.
func (s *Store) PipelinePolicyReviewRequirements(ctx context.Context, swarmID, candidateID string) ([]PipelinePolicyReviewRequirement, error) {
	if _, err := s.GetPipelinePolicyCandidateByID(ctx, swarmID, candidateID); err != nil {
		return nil, err
	}
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT swarm_id,candidate_id,rule_id,artifact,producer_uid,role FROM pipeline_policy_review_requirements WHERE swarm_id=? AND candidate_id=? ORDER BY rule_id`, swarmID, candidateID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PipelinePolicyReviewRequirement
	for rows.Next() {
		var r PipelinePolicyReviewRequirement
		if err := rows.Scan(&r.SwarmID, &r.CandidateID, &r.RuleID, &r.Artifact, &r.ProducerUID, &r.Role); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) nextPolicyReviewRole(ctx context.Context, c PipelinePolicyCandidate) (string, error) {
	w, err := s.GetSwarm(ctx, c.Preview.SwarmID)
	if err != nil {
		return "", err
	}
	graph, err := pipeline.Parse(string(w.Graph), "pipeline")
	if err != nil {
		return "", err
	}
	n := graph.Nodes[c.Preview.Transition.Next]
	if n.Type == "actor" && len(n.ActionClasses) == 1 && n.ActionClasses[0] == "review" {
		return n.Role, nil
	}
	return "", nil
}

// Для prospective review проверяется условие путей по реальному artifact.
// Класс будущего действия проверится на его границе; review не становится
// разрешением landing. Несовпавшие пути не создают независимость заранее.
func policyReviewArtifactApplies(rule workflowpolicy.Rule, e workflowpolicy.Evidence) bool {
	if rule.Require != workflowpolicy.IndependentReview {
		return false
	}
	if len(rule.Paths) == 0 {
		return true
	}
	rule.Actions = nil
	policy, err := workflowpolicy.New([]workflowpolicy.Rule{rule})
	if err != nil {
		return true
	}
	decision, err := policy.Evaluate(workflowpolicy.Input{TargetKind: "actor", ActionsKnown: true, Actions: []string{"review"}, Evidence: e})
	return err != nil || len(decision.Review) > 0
}

// Prospective authority follows only declared reachable actions in the immutable
// graph. It does not invent an action class for an unknown node.
func (s *Store) prospectivePolicyReviewApplies(ctx context.Context, c PipelinePolicyCandidate, rule workflowpolicy.Rule) (bool, error) {
	if !policyReviewArtifactApplies(rule, c.Preview.Input.Evidence) {
		return false, nil
	}
	if len(rule.Actions) == 0 {
		return true, nil
	}
	w, err := s.GetSwarm(ctx, c.Preview.SwarmID)
	if err != nil {
		return false, err
	}
	graph, err := pipeline.Parse(string(w.Graph), "pipeline")
	if err != nil {
		return false, err
	}
	pending := []string{c.Preview.Transition.Next}
	seen := map[string]bool{}
	for len(pending) > 0 {
		id := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if seen[id] {
			continue
		}
		seen[id] = true
		node := graph.Nodes[id]
		if node.Type == "actor" {
			for _, action := range node.ActionClasses {
				if slices.Contains(rule.Actions, action) {
					return true, nil
				}
			}
		}
		for _, edge := range graph.Edges {
			if edge.From == id {
				pending = append(pending, edge.To)
			}
		}
	}
	return false, nil
}

func (s *Store) retainPipelinePolicyReviewRequirements(ctx context.Context, c PipelinePolicyCandidate) error {
	role, err := s.nextPolicyReviewRole(ctx, c)
	if err != nil {
		return err
	}
	e := c.Preview.Input.Evidence
	if !e.Known || !e.Complete || e.Artifact == "" || e.ProducerUID == "" {
		return nil
	}
	for _, rule := range c.Preview.Policy.Rules {
		applies, err := s.prospectivePolicyReviewApplies(ctx, c, rule)
		if err != nil {
			return err
		}
		if !applies || (!slices.Contains(c.Preview.Decision.Review, rule.ID) && (role == "" || rule.ReviewRole != role)) {
			continue
		}
		var exists bool
		if err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pipeline_policy_review_requirements WHERE candidate_id=? AND rule_id=?)`, c.ID, rule.ID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		if _, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO pipeline_policy_review_requirements VALUES(?,?,?,?,?,?)`, c.ID, rule.ID, c.Preview.SwarmID, e.Artifact, e.ProducerUID, rule.ReviewRole); err != nil {
			return err
		}
	}
	return nil
}

// GetPipelinePolicyReviewAssignment reads assignment history only within its swarm.
func (s *Store) GetPipelinePolicyReviewAssignment(ctx context.Context, swarmID, candidateID, ruleID string) (PipelinePolicyReviewAssignment, error) {
	if _, err := s.GetPipelinePolicyCandidateByID(ctx, swarmID, candidateID); err != nil {
		return PipelinePolicyReviewAssignment{}, err
	}
	var a PipelinePolicyReviewAssignment
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT candidate_id,rule_id,review_swarm_id,review_sequence,actor_uid,incarnation FROM pipeline_policy_review_assignments WHERE candidate_id=? AND rule_id=?`, candidateID, ruleID).Scan(&a.CandidateID, &a.RuleID, &a.ReviewSwarmID, &a.ReviewSequence, &a.ActorUID, &a.Incarnation)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return a, err
}

func (s *Store) policyReviewBinding(ctx context.Context, c PipelinePolicyCandidate) (SwarmRoleBinding, error) {
	role, err := s.nextPolicyReviewRole(ctx, c)
	if err != nil {
		return SwarmRoleBinding{}, err
	}
	if role == "" {
		return SwarmRoleBinding{}, ErrSwarmRoleMismatch
	}
	b, err := s.stepRoleBinding(ctx, c.Preview.SwarmID, role)
	if err != nil {
		return b, err
	}
	if b.ActorUID == c.Preview.Input.Evidence.ProducerUID {
		return b, ErrSwarmRoleMismatch
	}
	if err := s.requireRoleActor(ctx, c.Preview.SwarmID, role, b.ActorUID, b.ActorIncarnation); err != nil {
		return b, err
	}
	var actor string
	var incarnation int64
	err = s.conn(ctx).QueryRowContext(ctx, `SELECT actor_uid,incarnation FROM pipeline_policy_review_assignments WHERE candidate_id=? ORDER BY rule_id LIMIT 1`, c.ID).Scan(&actor, &incarnation)
	if err == nil && (actor != b.ActorUID || incarnation != b.ActorIncarnation) {
		return b, fmt.Errorf("%w: assigned review actor or incarnation changed", ErrSwarmRoleMismatch)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return b, err
	}
	return b, s.guardOwnerIntervention(ctx, b.ActorUID)
}

// AssignPipelinePolicyReview accepts only the same pass's actual next review visit.
// Normally Store invokes this while applying the predecessor or reserving entry.
// Callers establish current external scope first; this API allocates no resource.
func (s *Store) AssignPipelinePolicyReview(ctx context.Context, swarmID, candidateID, ruleID, reviewSwarmID string, reviewSequence int64) (PipelinePolicyReviewAssignment, error) {
	var out PipelinePolicyReviewAssignment
	err := s.WithTx(ctx, func(ctx context.Context) error {
		c, err := s.GetPipelinePolicyCandidateByID(ctx, swarmID, candidateID)
		if err != nil {
			return err
		}
		rs, err := s.PipelinePolicyReviewRequirements(ctx, swarmID, candidateID)
		if err != nil {
			return err
		}
		var r PipelinePolicyReviewRequirement
		for _, v := range rs {
			if v.RuleID == ruleID {
				r = v
			}
		}
		if r.RuleID == "" {
			return ErrNotFound
		}
		w, err := s.GetSwarm(ctx, swarmID)
		if err != nil {
			return err
		}
		v, err := s.GetSwarmStep(ctx, reviewSwarmID, reviewSequence)
		if err != nil {
			return err
		}
		expected := c.Preview.Sequence
		if c.Boundary == "result" {
			expected++
		}
		if swarmID != reviewSwarmID || reviewSequence != expected || v.Pass != c.Preview.Policy.Pass || v.NodeID != c.Preview.Transition.Next || v.State != "pending" || v.ActorUID != "" || w.State != "active" || !activeExecution(w.Execution) {
			return ErrPipelineVisitConflict
		}
		var latest int64
		if err := s.conn(ctx).QueryRowContext(ctx, `SELECT MAX(sequence) FROM swarm_steps WHERE swarm_id=?`, swarmID).Scan(&latest); err != nil {
			return err
		}
		if latest != reviewSequence {
			return ErrPipelineVisitConflict
		}
		role, err := s.nextPolicyReviewRole(ctx, c)
		if err != nil {
			return err
		}
		if role != r.Role {
			return ErrSwarmRoleMismatch
		}
		b, err := s.policyReviewBinding(ctx, c)
		if err != nil {
			return err
		}
		out = PipelinePolicyReviewAssignment{candidateID, ruleID, swarmID, b.ActorUID, reviewSequence, b.ActorIncarnation}
		prior, err := s.GetPipelinePolicyReviewAssignment(ctx, swarmID, candidateID, ruleID)
		if err == nil {
			if prior != out {
				return ErrPipelineVisitConflict
			}
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		// Подтверждение принадлежит ровно этому predecessor/entry, не другому pass.
		d, rejected, err := s.pipelinePolicyCandidateDecision(ctx, c)
		if err != nil {
			return err
		}
		if rejected || (d.State != workflowpolicy.Allowed && !s.pipelinePolicyReviewEntryAllowed(ctx, c, d)) {
			return ErrPipelinePolicyPending
		}
		_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO pipeline_policy_review_assignments VALUES(?,?,?,?,?,?)`, candidateID, ruleID, swarmID, reviewSequence, b.ActorUID, b.ActorIncarnation)
		return err
	})
	return out, err
}

func (s *Store) assignInlinePolicyReview(ctx context.Context, c PipelinePolicyCandidate, sequence int64) error {
	role, err := s.nextPolicyReviewRole(ctx, c)
	if err != nil || role == "" {
		return err
	}
	rs, err := s.PipelinePolicyReviewRequirements(ctx, c.Preview.SwarmID, c.ID)
	if err != nil {
		return err
	}
	for _, r := range rs {
		if r.Role == role {
			if _, err := s.AssignPipelinePolicyReview(ctx, r.SwarmID, c.ID, r.RuleID, r.SwarmID, sequence); err != nil {
				return err
			}
		}
	}
	return nil
}

// inlinePolicyReview reads assignment history for exactly this visit. All rules
// share one pinned artifact/actor, enforced by creation from the same candidate.
func (s *Store) inlinePolicyReview(ctx context.Context, swarmID string, sequence int64) (PipelinePolicyReviewRequirement, PipelinePolicyReviewAssignment, error) {
	var candidateID, ruleID string
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT candidate_id,rule_id FROM pipeline_policy_review_assignments WHERE review_swarm_id=? AND review_sequence=? ORDER BY rule_id LIMIT 1`, swarmID, sequence).Scan(&candidateID, &ruleID)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	if err != nil {
		return PipelinePolicyReviewRequirement{}, PipelinePolicyReviewAssignment{}, err
	}
	rs, err := s.PipelinePolicyReviewRequirements(ctx, swarmID, candidateID)
	if err != nil {
		return PipelinePolicyReviewRequirement{}, PipelinePolicyReviewAssignment{}, err
	}
	a, err := s.GetPipelinePolicyReviewAssignment(ctx, swarmID, candidateID, ruleID)
	if err != nil {
		return PipelinePolicyReviewRequirement{}, a, err
	}
	for _, r := range rs {
		if r.RuleID == ruleID {
			return r, a, nil
		}
	}
	return PipelinePolicyReviewRequirement{}, a, ErrNotFound
}

// PipelineActorStepSource selects the workspace source for preparation and root
// result collection. The pass source/goal remain immutable. A review assignment
// overrides only its visit's commit with the exact verified work artifact.
// Unassigned required reviews return ErrPipelinePolicyPending, never the original
// pass commit as a substitute artifact. Read after reservation for preparation.
// Callers still perform ordinary external authority and final transport checks.
func (s *Store) PipelineActorStepSource(ctx context.Context, swarmID string, sequence int64) (PipelineSource, error) {
	var source PipelineSource
	err := s.WithTx(ctx, func(ctx context.Context) error {
		var err error
		source, err = s.pipelineActorStepSource(ctx, swarmID, sequence)
		return err
	})
	return source, err
}

func (s *Store) pipelineActorStepSource(ctx context.Context, swarmID string, sequence int64) (PipelineSource, error) {
	w, err := s.GetSwarm(ctx, swarmID)
	if err != nil {
		return PipelineSource{}, err
	}
	if w.Source == nil {
		return PipelineSource{}, ErrPipelineSourceUnverified
	}
	_, _, commit, err := s.stepPass(ctx, swarmID, sequence)
	if err != nil {
		return PipelineSource{}, err
	}
	r, _, err := s.inlinePolicyReview(ctx, swarmID, sequence)
	if err == nil {
		commit = r.Artifact
	} else if !errors.Is(err, ErrNotFound) {
		return PipelineSource{}, err
	} else {
		visit, err := s.GetSwarmStep(ctx, swarmID, sequence)
		if err != nil {
			return PipelineSource{}, err
		}
		graph, err := pipeline.Parse(string(w.Graph), "pipeline")
		if err != nil {
			return PipelineSource{}, err
		}
		node := graph.Nodes[visit.NodeID]
		if node.Type == "actor" && len(node.ActionClasses) == 1 && node.ActionClasses[0] == "review" {
			policy, err := s.SwarmStepWorkflowPolicy(ctx, swarmID, sequence)
			if err != nil {
				return PipelineSource{}, err
			}
			hasReviewRule := false
			for _, rule := range policy.Rules {
				hasReviewRule = hasReviewRule || rule.Require == workflowpolicy.IndependentReview
			}
			if hasReviewRule {
				preview, _, err := s.actorEntryPolicyPreview(ctx, w, graph, sequence)
				if err != nil {
					return PipelineSource{}, err
				}
				if preview.Decision.State == workflowpolicy.Blocked {
					return PipelineSource{}, ErrPipelinePolicyPending
				}
				for _, rule := range policy.Rules {
					applies, err := s.prospectivePolicyReviewApplies(ctx, PipelinePolicyCandidate{Preview: preview}, rule)
					if err != nil {
						return PipelineSource{}, err
					}
					if applies {
						return PipelineSource{}, ErrPipelinePolicyPending
					}
				}
			}
		}
	}
	return PipelineSource{Origin: w.Source.Origin, Commit: commit}, nil
}

// CheckPipelinePolicyReviewAssignment is a no-effect final fence, not permission
// to resend unknown delivery or replace an assigned actor.
func (s *Store) CheckPipelinePolicyReviewAssignment(ctx context.Context, swarmID, candidateID, ruleID string) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		a, err := s.GetPipelinePolicyReviewAssignment(ctx, swarmID, candidateID, ruleID)
		if err != nil {
			return err
		}
		rs, err := s.PipelinePolicyReviewRequirements(ctx, swarmID, candidateID)
		if err != nil {
			return err
		}
		var r PipelinePolicyReviewRequirement
		for _, v := range rs {
			if v.RuleID == ruleID {
				r = v
			}
		}
		if r.RuleID == "" || a.ReviewSwarmID != swarmID || a.ActorUID == r.ProducerUID {
			return ErrSwarmRoleMismatch
		}
		if err := s.requireRoleActor(ctx, swarmID, r.Role, a.ActorUID, a.Incarnation); err != nil {
			return err
		}
		if err := s.guardOwnerIntervention(ctx, a.ActorUID); err != nil {
			return err
		}
		interrupted, err := s.interruptedStep(ctx, swarmID, a.ReviewSequence)
		if err != nil {
			return err
		}
		if interrupted {
			return ErrPipelineVisitInterrupted
		}
		w, err := s.GetSwarm(ctx, swarmID)
		if err != nil {
			return err
		}
		v, err := s.GetSwarmStep(ctx, swarmID, a.ReviewSequence)
		if err != nil {
			return err
		}
		var latest int64
		if err := s.conn(ctx).QueryRowContext(ctx, `SELECT MAX(sequence) FROM swarm_steps WHERE swarm_id=?`, swarmID).Scan(&latest); err != nil {
			return err
		}
		if w.State != "active" || !activeExecution(w.Execution) || latest != a.ReviewSequence || (v.State != "pending" && v.State != "attesting" && v.State != "delivering" && v.State != "inflight") {
			return ErrPipelineVisitConflict
		}
		if v.State != "pending" && (v.ActorUID != a.ActorUID || v.ActorIncarnation != a.Incarnation) {
			return ErrPipelineVisitConflict
		}
		c, err := s.GetPipelinePolicyCandidateByID(ctx, swarmID, candidateID)
		if err != nil {
			return err
		}
		d, rejected, err := s.pipelinePolicyCandidateDecision(ctx, c)
		if err != nil {
			return err
		}
		if rejected || (d.State != workflowpolicy.Allowed && !s.pipelinePolicyReviewEntryAllowed(ctx, c, d)) {
			return ErrPipelinePolicyPending
		}
		return nil
	})
}

func (s *Store) checkedPipelinePolicyReview(ctx context.Context, r PipelinePolicyReviewRequirement, a PipelinePolicyReviewAssignment) (string, workflowpolicy.ReviewEvidence, error) {
	fail := func(err error) (string, workflowpolicy.ReviewEvidence, error) {
		return "", workflowpolicy.ReviewEvidence{}, err
	}
	if a.ReviewSwarmID != r.SwarmID || a.ActorUID == r.ProducerUID {
		return fail(ErrSwarmRoleMismatch)
	}
	if err := s.requireRoleActor(ctx, r.SwarmID, r.Role, a.ActorUID, a.Incarnation); err != nil {
		return fail(err)
	}
	if err := s.guardOwnerIntervention(ctx, a.ActorUID); err != nil {
		return fail(err)
	}
	i, err := s.GetPipelineCompletionIntent(ctx, r.SwarmID, a.ReviewSequence)
	if err != nil {
		return fail(err)
	}
	if err := s.requireCompletionReceipt(ctx, i); err != nil {
		return fail(err)
	}
	checked, err := s.checkActorCompletion(ctx, i.ActorCompletion)
	if err != nil {
		return fail(err)
	}
	if !checked.replay {
		if err := s.checkResultIntent(ctx, i); err != nil {
			return fail(err)
		}
	}
	if i.ActorUID != a.ActorUID || i.Incarnation != a.Incarnation || i.BaseCommit != r.Artifact || i.Outcome != "success" {
		return fail(ErrPipelineVisitConflict)
	}
	o, err := s.GetPipelineResultObservation(ctx, r.SwarmID, a.ReviewSequence)
	if err != nil {
		return fail(err)
	}
	e, err := s.GetPipelineResultEvidence(ctx, r.SwarmID, a.ReviewSequence)
	if err != nil {
		return fail(err)
	}
	if o.State != "observed" || o.Intent != i || e.ResultRequest != o.Request || e.Head != r.Artifact || len(e.Paths) != 0 {
		return fail(ErrPipelineVisitConflict)
	}
	return o.ID, workflowpolicy.ReviewEvidence{Artifact: r.Artifact, ActorUID: a.ActorUID, Role: r.Role, Success: true}, nil
}

// RecordPipelinePolicyReviewEvidence records only an acknowledged, root-verified,
// unchanged successful inline review. It cannot import a separate swarm's review.
func (s *Store) RecordPipelinePolicyReviewEvidence(ctx context.Context, swarmID, candidateID, ruleID string) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		rs, err := s.PipelinePolicyReviewRequirements(ctx, swarmID, candidateID)
		if err != nil {
			return err
		}
		var r PipelinePolicyReviewRequirement
		for _, v := range rs {
			if v.RuleID == ruleID {
				r = v
			}
		}
		if r.RuleID == "" {
			return ErrNotFound
		}
		a, err := s.GetPipelinePolicyReviewAssignment(ctx, swarmID, candidateID, ruleID)
		if err != nil {
			return err
		}
		id, _, err := s.checkedPipelinePolicyReview(ctx, r, a)
		if err != nil {
			return err
		}
		var prior string
		err = s.conn(ctx).QueryRowContext(ctx, `SELECT observation_id FROM pipeline_policy_review_evidence WHERE candidate_id=? AND rule_id=?`, candidateID, ruleID).Scan(&prior)
		if err == nil {
			if prior != id {
				return ErrPipelineVisitConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO pipeline_policy_review_evidence VALUES(?,?,?)`, candidateID, ruleID, id)
		return err
	})
}

func (s *Store) pipelinePolicyReviews(ctx context.Context, c PipelinePolicyCandidate) ([]workflowpolicy.ReviewEvidence, error) {
	// Только прошлые/текущие реальные graph reviews этого pass, не future evidence.
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT DISTINCT a.candidate_id,a.rule_id FROM pipeline_policy_review_assignments a JOIN pipeline_policy_candidates c ON c.id=a.candidate_id JOIN pipeline_result_observations o ON o.swarm_id=a.review_swarm_id AND o.sequence=a.review_sequence AND o.state='observed' WHERE a.review_swarm_id=? AND c.pass_sequence=? AND a.review_sequence<=?
 AND NOT EXISTS(SELECT 1 FROM pipeline_completion_intents work WHERE work.swarm_id=a.review_swarm_id AND work.sequence>a.review_sequence AND work.sequence<=? AND NOT EXISTS(SELECT 1 FROM pipeline_policy_review_assignments next_review WHERE next_review.review_swarm_id=work.swarm_id AND next_review.review_sequence=work.sequence))`, c.Preview.SwarmID, c.Preview.Policy.Pass, c.Preview.Sequence, c.Preview.Sequence)
	if err != nil {
		return nil, err
	}
	type key struct{ c, r string }
	var keys []key
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.c, &k.r); err != nil {
			rows.Close()
			return nil, err
		}
		keys = append(keys, k)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var out []workflowpolicy.ReviewEvidence
	for _, k := range keys {
		rs, err := s.PipelinePolicyReviewRequirements(ctx, c.Preview.SwarmID, k.c)
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			if r.RuleID != k.r || r.Artifact != c.Preview.Input.Evidence.Artifact || r.ProducerUID != c.Preview.Input.Evidence.ProducerUID {
				continue
			}
			a, err := s.GetPipelinePolicyReviewAssignment(ctx, r.SwarmID, k.c, k.r)
			if err != nil {
				return nil, err
			}
			if err := s.requireRoleActor(ctx, r.SwarmID, r.Role, a.ActorUID, a.Incarnation); err != nil {
				return nil, fmt.Errorf("%w: %v", errPolicyReviewUnavailable, err)
			}
			_, e, err := s.checkedPipelinePolicyReview(ctx, r, a)
			if err != nil {
				return nil, err
			}
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *Store) retainInlinePolicyReviewEvidence(ctx context.Context, swarmID string, sequence int64) error {
	r, _, err := s.inlinePolicyReview(ctx, swarmID, sequence)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	requirements, err := s.PipelinePolicyReviewRequirements(ctx, swarmID, r.CandidateID)
	if err != nil {
		return err
	}
	for _, requirement := range requirements {
		if requirement.Role == r.Role {
			if err := s.RecordPipelinePolicyReviewEvidence(ctx, swarmID, r.CandidateID, requirement.RuleID); err != nil {
				return err
			}
		}
	}
	return nil
}

// policyReviewFailure derives terminal inability from the current graph and
// durable authority. Unknown evidence/delivery is not a negative review.
func (s *Store) policyReviewFailure(ctx context.Context, c PipelinePolicyCandidate, d workflowpolicy.Decision) (string, error) {
	if d.State == workflowpolicy.Blocked {
		return "", nil
	}
	role, err := s.nextPolicyReviewRole(ctx, c)
	if err != nil {
		return "", err
	}
	if role != "" {
		configured, hasReviewRule := false, false
		for _, rule := range c.Preview.Policy.Rules {
			applies, err := s.prospectivePolicyReviewApplies(ctx, c, rule)
			if err != nil {
				return "", err
			}
			hasReviewRule = hasReviewRule || applies
			configured = configured || applies && rule.ReviewRole == role
		}
		if hasReviewRule && !configured {
			return fmt.Sprintf("independent review unavailable: next graph role %q is not a configured review role", role), nil
		}
		if configured && c.Preview.Input.Evidence.Known && c.Preview.Input.Evidence.Complete {
			if _, err := s.policyReviewBinding(ctx, c); err != nil {
				return fmt.Sprintf("independent review unavailable for role %q: %v", role, err), nil
			}
		}
	}
	if len(d.Review) > 0 && !s.pipelinePolicyReviewEntryAllowed(ctx, c, d) {
		return "independent review unavailable: graph has no valid next reviewer for rules: " + fmt.Sprint(d.Review), nil
	}
	return "", nil
}

// A waiting review obligation permits only dispatch of its actual next reviewer,
// not landing or general work. The obligation remains waiting, not fake success.
func (s *Store) pipelinePolicyReviewEntryAllowed(ctx context.Context, c PipelinePolicyCandidate, d workflowpolicy.Decision) bool {
	if d.State != workflowpolicy.WaitingReview || len(d.Review) == 0 || len(d.Human) > 0 || len(d.Unknown) > 0 || len(d.Denied) > 0 {
		return false
	}
	e := c.Preview.Input.Evidence
	if !e.Known || !e.Complete || e.Artifact == "" || e.ProducerUID == "" {
		return false
	}
	role, err := s.nextPolicyReviewRole(ctx, c)
	if err != nil || role == "" {
		return false
	}
	for _, id := range d.Review {
		matched := false
		for _, r := range c.Preview.Policy.Rules {
			if r.ID == id && r.Require == workflowpolicy.IndependentReview && r.ReviewRole == role {
				matched = true
			}
		}
		if !matched {
			return false
		}
	}
	_, err = s.policyReviewBinding(ctx, c)
	return err == nil
}

// policyArtifactEvidence preserves the original work lineage through an unchanged
// review. Merely observing H does not make the reviewer its author. Work steps,
// including work that changes H, never receive this inheritance.
func (s *Store) policyArtifactEvidence(ctx context.Context, swarmID string, sequence int64, e workflowpolicy.Evidence) (workflowpolicy.Evidence, error) {
	r, _, err := s.inlinePolicyReview(ctx, swarmID, sequence)
	if errors.Is(err, ErrNotFound) {
		return e, nil
	}
	if err != nil {
		return e, err
	}
	if e.Artifact != r.Artifact || len(e.Paths) != 0 {
		return e, nil
	}
	c, err := s.GetPipelinePolicyCandidateByID(ctx, swarmID, r.CandidateID)
	if err != nil {
		return e, err
	}
	return c.Preview.Input.Evidence, nil
}
