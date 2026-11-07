package server

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/pipeline"
	"brood.dev/go/brood/piprender"
	"brood.dev/go/brood/store"
)

func (s *Server) workflowProjectVisible(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Cache-Control", "no-store")
	user, ok := currentUser(w, r)
	if !ok {
		return false
	}
	allowed, err := s.app.Can(user.ID, app.ActionViewProject, app.ProjectRes(r.PathValue("id")))
	if err != nil {
		apiError(w, r, err)
		return false
	}
	if !allowed {
		writeDetail(w, http.StatusNotFound, "project not found")
		return false
	}
	return true
}

func (s *Server) handleWorkflowCatalog(w http.ResponseWriter, r *http.Request) {
	if !s.workflowProjectVisible(w, r) {
		return
	}
	definitions, err := s.store.ListWorkflowDefinitions(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDetail(w, http.StatusServiceUnavailable, "workflows unavailable")
		return
	}
	type item struct {
		Name        string  `json:"name"`
		Version     int64   `json:"version"`
		Revision    string  `json:"revision"`
		PublishedAt *string `json:"publishedAt"`
	}
	items := make([]item, 0, len(definitions))
	for _, d := range definitions {
		var published *string
		if d.PublishedAt.Valid {
			published = &d.PublishedAt.String
		}
		items = append(items, item{d.Name, d.Version, d.Revision, published})
	}
	writeJSON(w, http.StatusOK, map[string]any{"workflows": items})
}

func (s *Server) handleWorkflowGraph(w http.ResponseWriter, r *http.Request) {
	if !s.workflowProjectVisible(w, r) {
		return
	}
	var expected int64
	var revision string
	versions, hasVersion := r.URL.Query()["version"]
	revisions, hasRevision := r.URL.Query()["revision"]
	if hasVersion != hasRevision {
		writeDetail(w, http.StatusBadRequest, "workflow version and revision must be supplied together")
		return
	}
	if hasVersion {
		var err error
		if len(versions) != 1 {
			writeDetail(w, http.StatusBadRequest, "invalid workflow version")
			return
		}
		expected, err = strconv.ParseInt(versions[0], 10, 64)
		if err != nil || expected < 1 {
			writeDetail(w, http.StatusBadRequest, "invalid workflow version")
			return
		}
		if len(revisions) != 1 {
			writeDetail(w, http.StatusBadRequest, "invalid workflow revision")
			return
		}
		revision = revisions[0]
		decoded, err := hex.DecodeString(revision)
		if err != nil || len(decoded) != 16 || hex.EncodeToString(decoded) != revision {
			writeDetail(w, http.StatusBadRequest, "invalid workflow revision")
			return
		}
	}
	definition, err := s.store.GetWorkflowDefinition(r.Context(), r.PathValue("id"), r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		writeDetail(w, http.StatusNotFound, "workflow not found")
		return
	}
	if err != nil {
		writeDetail(w, http.StatusServiceUnavailable, "workflows unavailable")
		return
	}
	if hasVersion && (expected != definition.Version || revision != definition.Revision) {
		writeDetail(w, http.StatusConflict, "workflow version changed")
		return
	}
	doc, err := pipeline.Parse(definition.YAML, definition.Name+".yaml")
	if err != nil {
		apiError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"svg": piprender.SVG(piprender.RunView{Graph: doc.Graph()})})
}

func (s *Server) handleWorkflows(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	allowed, err := s.app.Can(user.ID, app.ActionViewProject, app.ProjectRes(id))
	if err != nil {
		apiError(w, r, err)
		return
	}
	if !allowed {
		writeDetail(w, http.StatusNotFound, "project not found")
		return
	}
	if r.Method == http.MethodGet {
		definitions, err := s.store.ListWorkflowDefinitions(r.Context(), id)
		if err != nil {
			writeDetail(w, http.StatusServiceUnavailable, "workflows unavailable")
			return
		}
		result := make(map[string]string, len(definitions))
		for _, d := range definitions {
			result[d.Name] = d.YAML
		}
		writeJSON(w, http.StatusOK, map[string]any{"workflows": result})
		return
	}
	definitions, err := readWorkflows(w, r)
	if err != nil {
		writeDetail(w, http.StatusBadRequest, "invalid or oversized workflow request")
		return
	}
	_, err = s.store.SyncWorkflowDefinitions(r.Context(), id, user.ID, definitions, func(ctx context.Context) bool {
		return secretRequestSession(ctx, s, r, user.ID)
	})
	switch {
	case errors.Is(err, store.ErrProjectSyncDenied):
		writeDetail(w, http.StatusForbidden, "workflow authorization changed or access denied")
	case errors.Is(err, store.ErrNotFound):
		writeDetail(w, http.StatusNotFound, "project not found")
	case errors.Is(err, store.ErrWorkflowLimit):
		writeDetail(w, http.StatusBadRequest, err.Error())
	case err != nil:
		writeDetail(w, http.StatusServiceUnavailable, "workflow sync failed")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) handleWorkflowDelete(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	allowed, err := s.app.Can(user.ID, app.ActionViewProject, app.ProjectRes(id))
	if err != nil {
		apiError(w, r, err)
		return
	}
	if !allowed {
		writeDetail(w, http.StatusNotFound, "project not found")
		return
	}
	err = s.store.DeleteWorkflowDefinition(r.Context(), id, user.ID, r.PathValue("name"), func(ctx context.Context) bool {
		return secretRequestSession(ctx, s, r, user.ID)
	})
	switch {
	case errors.Is(err, store.ErrProjectSyncDenied):
		writeDetail(w, http.StatusForbidden, "workflow authorization changed or access denied")
	case errors.Is(err, store.ErrNotFound):
		writeDetail(w, http.StatusNotFound, "workflow not found")
	case errors.Is(err, store.ErrWorkflowRunsExist):
		writeDetail(w, http.StatusConflict, err.Error())
	case err != nil:
		writeDetail(w, http.StatusServiceUnavailable, "workflow deletion failed")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func readWorkflows(w http.ResponseWriter, r *http.Request) (map[string]string, error) {
	invalid := errors.New("invalid workflows")
	control := http.NewResponseController(w)
	_ = control.SetReadDeadline(time.Now().Add(15 * time.Second))
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, pipeline.MaxWorkflowBodyBytes))
	_ = control.SetReadDeadline(time.Time{})
	if err != nil || !utf8.Valid(data) {
		return nil, invalid
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if uniqueSyncJSON(d) != nil {
		return nil, invalid
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, invalid
	}
	body, err := syncObject(data, map[string]bool{"workflows": true})
	if err != nil || len(body) != 1 {
		return nil, invalid
	}
	raw := bytes.TrimSpace(body["workflows"])
	var values map[string]json.RawMessage
	if len(raw) == 0 || raw[0] != '{' || json.Unmarshal(raw, &values) != nil || values == nil || len(values) > pipeline.MaxWorkflowDefinitions {
		return nil, invalid
	}
	definitions := make(map[string]string, len(values))
	total := 0
	for name, raw := range values {
		var text string
		if !syncString(raw, &text) {
			return nil, invalid
		}
		total += len(name) + len(text)
		if len(name) > pipeline.MaxWorkflowNameBytes || len(text) > pipeline.MaxWorkflowDefinitionBytes || total > pipeline.MaxWorkflowBatchBytes {
			return nil, invalid
		}
		doc, err := pipeline.Parse(text, name+".yaml")
		if err != nil || doc.Name != name || doc.Validate() != nil {
			return nil, invalid
		}
		definitions[name] = text
	}
	return definitions, nil
}
