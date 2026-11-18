package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
	"unicode/utf8"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/store"
	"brood.dev/go/brood/workflowpolicy"
)

const workflowPolicyBodyLimit = 1 << 20

type workflowPolicyJSON struct {
	ProjectID string                `json:"projectId"`
	Revision  int64                 `json:"revision"`
	Rules     []workflowpolicy.Rule `json:"rules"`
	CreatedBy string                `json:"createdBy"`
	CreatedAt string                `json:"createdAt"`
}

type workflowPolicyMutation struct {
	Revision *int64                `json:"revision"`
	Rules    []workflowpolicy.Rule `json:"rules"`
}

func (s *Server) mountWorkflowPolicy() {
	s.mux.HandleFunc("GET /api/projects/{id}/workflow-policy", s.handleWorkflowPolicy)
	s.mux.HandleFunc("PUT /api/projects/{id}/workflow-policy", s.handleWorkflowPolicyUpdate)
}

func writeWorkflowPolicy(w http.ResponseWriter, policy store.WorkflowPolicyVersion) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, workflowPolicyJSON{ProjectID: policy.ProjectID, Revision: policy.Revision, Rules: policy.Rules, CreatedBy: policy.CreatedBy, CreatedAt: policy.CreatedAt})
}

func workflowPolicyError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeDetail(w, http.StatusNotFound, "project not found")
	case errors.Is(err, store.ErrWorkflowPolicyConflict):
		writeDetail(w, http.StatusConflict, "workflow policy revision changed; reload before editing")
	case errors.Is(err, store.ErrWorkflowPolicyDenied):
		writeDetail(w, http.StatusForbidden, "workflow policy authorization changed or access denied")
	default:
		requestLogger(r.Context()).Error("project workflow policy failed", "err", err.Error())
		writeDetail(w, http.StatusInternalServerError, "workflow policy service unavailable")
	}
}

func (s *Server) handleWorkflowPolicy(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	allowed, err := s.app.Can(user.ID, app.ActionViewProject, app.ProjectRes(r.PathValue("id")))
	if err != nil {
		apiError(w, r, err)
		return
	}
	if !allowed {
		writeDetail(w, http.StatusNotFound, "project not found")
		return
	}
	policy, err := s.store.ProjectWorkflowPolicy(r.Context(), r.PathValue("id"))
	if err != nil {
		workflowPolicyError(w, r, err)
		return
	}
	writeWorkflowPolicy(w, policy)
}

func (s *Server) handleWorkflowPolicyUpdate(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	allowed, err := s.store.CanManageProjectWorkflowPolicy(r.Context(), user.ID, r.PathValue("id"))
	if err != nil {
		workflowPolicyError(w, r, err)
		return
	}
	if !allowed {
		writeDetail(w, http.StatusForbidden, "workflow policy authorization changed or access denied")
		return
	}
	input, ok := readWorkflowPolicyMutation(w, r)
	if !ok {
		return
	}
	if _, err := workflowpolicy.New(input.Rules); err != nil {
		writeDetail(w, http.StatusUnprocessableEntity, "invalid workflow policy rules")
		return
	}
	encoded, err := json.Marshal(input.Rules)
	if err != nil || len(encoded) > store.MaxWorkflowPolicyBytes {
		writeDetail(w, http.StatusUnprocessableEntity, "workflow policy exceeds storage limit")
		return
	}
	policy, err := s.store.SetProjectWorkflowPolicy(r.Context(), user.ID, r.PathValue("id"), *input.Revision, input.Rules, func(ctx context.Context) bool { return secretRequestSession(ctx, s, r, user.ID) })
	if err != nil {
		workflowPolicyError(w, r, err)
		return
	}
	writeWorkflowPolicy(w, policy)
}

func readWorkflowPolicyMutation(w http.ResponseWriter, r *http.Request) (workflowPolicyMutation, bool) {
	control := http.NewResponseController(w)
	_ = control.SetReadDeadline(time.Now().Add(15 * time.Second))
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, workflowPolicyBodyLimit))
	_ = control.SetReadDeadline(time.Time{})
	invalid := func() (workflowPolicyMutation, bool) {
		writeDetail(w, http.StatusBadRequest, "workflow policy request requires explicit revision and rules without unknown or duplicate fields")
		return workflowPolicyMutation{}, false
	}
	if err != nil || !utf8.Valid(data) {
		return invalid()
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	tokens := 0
	if !workflowPolicyJSONValue(d, 0, &tokens) {
		return invalid()
	}
	if _, err := d.Token(); err != io.EOF {
		return invalid()
	}
	var input workflowPolicyMutation
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || input.Revision == nil || input.Rules == nil {
		return invalid()
	}
	return input, true
}

func workflowPolicyJSONValue(d *json.Decoder, depth int, tokens *int) bool {
	*tokens++
	if depth > 8 || *tokens > 32768 {
		return false
	}
	value, err := d.Token()
	if err != nil || value == nil {
		return false
	}
	delimiter, composite := value.(json.Delim)
	if !composite {
		return true
	}
	switch delimiter {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			value, err := d.Token()
			key, ok := value.(string)
			if err != nil || !ok || seen[key] {
				return false
			}
			seen[key] = true
			if depth == 0 {
				if key != "revision" && key != "rules" {
					return false
				}
			} else if depth == 2 {
				switch key {
				case "id", "paths", "actions", "require", "reviewRole":
				default:
					return false
				}
			} else {
				return false
			}
			if !workflowPolicyJSONValue(d, depth+1, tokens) {
				return false
			}
		}
		end, err := d.Token()
		return err == nil && end == json.Delim('}')
	case '[':
		for d.More() {
			if !workflowPolicyJSONValue(d, depth+1, tokens) {
				return false
			}
		}
		end, err := d.Token()
		return err == nil && end == json.Delim(']')
	}
	return false
}
