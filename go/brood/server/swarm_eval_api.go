package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/store"
)

// evalScopes проверяет внешние namespace до FULL-транзакции.
func (s *Server) evalScopes(ctx context.Context, candidate, reference string) ([]store.Swarm, error) {
	var scopes []store.Swarm
	for _, id := range []string{candidate, reference} {
		sw, err := s.store.GetSwarm(ctx, id)
		if err != nil {
			return nil, err
		}
		if err = s.checkSwarmNamespace(ctx, toAppSwarm(sw)); err != nil {
			return nil, err
		}
		scopes = append(scopes, sw)
	}
	return scopes, nil
}

// evalAuthority читает только локальные данные в переданном контексте транзакции.
func (s *Server) evalAuthority(scopes []store.Swarm, session func(context.Context) bool, owner bool) store.SwarmEvalAuthorize {
	return func(ctx context.Context, action string, b store.SwarmEvalBinding) error {
		if session != nil && !session(ctx) {
			return store.ErrSwarmRequestDenied
		}
		if owner && action != "interrupt_eval_children" {
			return store.ErrSwarmRequestDenied
		}
		for i, id := range []string{b.CandidateSwarmID, b.ReferenceSwarmID} {
			if len(scopes) != 2 || scopes[i].ID != id {
				return store.ErrSwarmEvalConflict
			}
			current, err := s.store.GetSwarm(ctx, id)
			if err != nil {
				return err
			}
			old := toAppSwarm(scopes[i])
			now := toAppSwarm(current)
			if old.ProjectID != now.ProjectID || old.AtespaceUID != now.AtespaceUID || old.AtespaceName != now.AtespaceName {
				return store.ErrSwarmEvalConflict
			}
			if err = s.authorizeSwarmManagerIntent(ctx, b.LauncherID, id); err != nil {
				return err
			}
		}
		if owner {
			return s.authorizeSwarmOwnerIntent(ctx, b.LauncherID, b.CandidateSwarmID)
		}
		return nil
	}
}

type evalBindingOperation struct {
	server  *Server
	ctx     context.Context
	input   store.SwarmEvalBindingInput
	session func(context.Context) bool
	scopes  []store.Swarm
	result  store.SwarmEvalBinding
}

func (o *evalBindingOperation) Prepare(_ []app.Swarm) (app.WorkloadAdmission, error) {
	// Binding пишет только метаданные: TemplateOnly не резервирует акторов,
	// но удерживает ту же project gate, что native и managed admission.
	plan := app.WorkloadAdmission{Name: "eval-binding", TemplateOnly: true}
	var err error
	o.scopes, err = o.server.evalScopes(o.ctx, o.input.CandidateSwarmID, o.input.ReferenceSwarmID)
	if err != nil {
		return plan, err
	}
	_, err = o.server.store.GetSwarmEvalBinding(o.ctx, o.input.CandidateSwarmID)
	if err == nil {
		return plan, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return plan, err
	}
	reservations, err := nativeShardReservations(o.ctx, o.server.ateapi, []app.Swarm{toAppSwarm(o.scopes[0])})
	if err != nil {
		return plan, err
	}
	if len(reservations) != 0 {
		return plan, store.ErrSwarmEvalConflict
	}
	return plan, nil
}

func (o *evalBindingOperation) Apply(_ map[string]string) error {
	var err error
	o.result, err = o.server.store.CreateSwarmEvalBinding(o.ctx, o.input, o.server.evalAuthority(o.scopes, o.session, false))
	return err
}

func evalAPIError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrSwarmEvalConflict) {
		writeDetail(w, http.StatusConflict, err.Error())
		return
	}
	swarmExecutionError(w, r, err)
}

func (s *Server) handleSwarmEvalBind(w http.ResponseWriter, r *http.Request) {
	user, ok := s.visibleSwarmUser(w, r)
	if !ok {
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSwarmStartBody))
	fields, parseErr := proxmoxObject(string(data))
	var in store.SwarmEvalBindingInput
	if err != nil || parseErr != nil || len(fields) != 4 {
		writeDetail(w, 400, "expected {referenceSwarmId, referencePass, definition, model}")
		return
	}
	for key, raw := range fields {
		valid := false
		switch key {
		case "referenceSwarmId":
			valid = syncString(raw, &in.ReferenceSwarmID)
		case "definition":
			valid = syncString(raw, &in.Definition)
		case "model":
			valid = syncString(raw, &in.Model)
		case "referencePass":
			valid = string(raw) != "null" && json.Unmarshal(raw, &in.ReferencePass) == nil
		}
		if !valid {
			writeDetail(w, 400, "invalid eval selection")
			return
		}
	}
	if err = s.requireSwarmView(in.ReferenceSwarmID, user.ID); err != nil {
		evalAPIError(w, r, err)
		return
	}
	in.CandidateSwarmID, in.LauncherID = r.PathValue("id"), user.ID
	op := &evalBindingOperation{server: s, ctx: r.Context(), input: in, session: func(ctx context.Context) bool { return secretRequestSession(ctx, s, r, user.ID) }}
	if err = s.app.AdmitWorkloadContext(r.Context(), user.ID, in.CandidateSwarmID, op); err != nil {
		evalAPIError(w, r, err)
		return
	}
	s.writeSwarmEval(w, r, op.result)
}

func (s *Server) handleSwarmEvalGet(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.visibleSwarmUser(w, r); !ok {
		return
	}
	b, err := s.store.GetSwarmEvalBinding(r.Context(), r.PathValue("id"))
	if err != nil {
		evalAPIError(w, r, err)
		return
	}
	s.writeSwarmEval(w, r, b)
}

func (s *Server) writeSwarmEval(w http.ResponseWriter, r *http.Request, b store.SwarmEvalBinding) {
	sw, err := s.store.GetSwarm(r.Context(), b.CandidateSwarmID)
	if err != nil {
		evalAPIError(w, r, err)
		return
	}
	out := map[string]any{
		"swarmId":          b.CandidateSwarmID,
		"referenceSwarmId": b.ReferenceSwarmID,
		"referencePass":    b.ReferencePass,
		"definition":       swarmDefinitionJSON{Name: b.Definition, Version: b.DefinitionVersion, Revision: b.DefinitionRevision},
		"model":            b.Model,
		"source":           swarmSourceJSON{Origin: b.Origin, Commit: b.SourceCommit},
		"goal":             b.Goal,
		"nonProduction":    b.NonProduction,
		"execution":        sw.Execution,
		"replay":           nil,
	}
	replay, err := s.store.LatestSwarmEvalReplay(r.Context(), b.CandidateSwarmID)
	if err == nil {
		out["replay"] = map[string]any{"requestId": replay.RequestID, "state": replay.State, "reason": replay.Reason, "interruptedChildren": replay.InterruptedChildren}
	} else if !errors.Is(err, store.ErrNotFound) {
		evalAPIError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleSwarmEvalStart(w http.ResponseWriter, r *http.Request) {
	user, ok := s.visibleSwarmUser(w, r)
	if !ok {
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSwarmStartBody))
	empty := strings.Trim(string(data), " \t\r\n")
	fields, parseErr := proxmoxObject(empty)
	if err != nil || (empty != "" && empty != "null" && (parseErr != nil || len(fields) != 0)) {
		writeDetail(w, 400, "eval start takes no selection")
		return
	}
	b, err := s.store.GetSwarmEvalBinding(r.Context(), r.PathValue("id"))
	if err != nil {
		evalAPIError(w, r, err)
		return
	}
	scopes, err := s.evalScopes(r.Context(), b.CandidateSwarmID, b.ReferenceSwarmID)
	if err != nil {
		evalAPIError(w, r, err)
		return
	}
	sw, err := s.store.StartSwarmEval(r.Context(), b.CandidateSwarmID, user.ID, s.evalAuthority(scopes, func(ctx context.Context) bool { return secretRequestSession(ctx, s, r, user.ID) }, false))
	if err != nil {
		evalAPIError(w, r, err)
		return
	}
	s.writeSwarmExecution(w, r, sw)
	s.steps.kick(sw.ID)
}
