package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/gitorigin"
	"brood.dev/go/brood/pipeline"
	"brood.dev/go/brood/store"
)

const projectSyncBodyLimit = 64 << 10

type projectSyncJSON struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Repository string `json:"repository"`
	Created    bool   `json:"created"`
	Changed    bool   `json:"changed"`
	OriginMode string `json:"origin_mode"`
}

func (s *Server) handleProjectSync(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	declaration, err := readProjectSync(w, r)
	if err != nil {
		writeDetail(w, http.StatusBadRequest, "invalid or oversized project declaration")
		return
	}
	result, err := s.syncProject(r.Context(), user.ID, declaration, func(ctx context.Context) bool { return secretRequestSession(ctx, s, r, user.ID) })
	if errors.Is(err, store.ErrProjectSyncDenied) {
		writeDetail(w, http.StatusForbidden, "project sync authorization changed or access denied")
		return
	}
	if errors.Is(err, store.ErrWorkflowLimit) {
		writeDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		writeDetail(w, http.StatusServiceUnavailable, "project sync failed")
		return
	}
	mode := gitorigin.Mode(result.Repository, result.Repository, s.git.remotes.has, managedPublicHost)
	status := http.StatusOK
	if result.Created {
		status = http.StatusCreated
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, projectSyncJSON{ID: result.ID, Name: result.Name, Repository: result.Repository, Created: result.Created, Changed: result.Changed, OriginMode: mode})
}

func (s *Server) syncProject(ctx context.Context, userID string, declaration store.ProjectDeclaration, authorize func(context.Context) bool) (store.ProjectSyncResult, error) {
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return store.ProjectSyncResult{}, err
		}
		project, err := s.store.ProjectByName(ctx, declaration.Name)
		var result store.ProjectSyncResult
		write := func(expectedID string) error {
			return s.store.WithTx(ctx, func(txCtx context.Context) error {
				var err error
				result, err = s.store.SyncProject(txCtx, userID, expectedID, declaration, authorize)
				if err != nil {
					return err
				}
				if declaration.Workflows != nil {
					var changed bool
					changed, err = s.store.SyncWorkflowDefinitions(txCtx, result.ID, userID, declaration.Workflows, authorize)
					result.Changed = result.Changed || changed
				}
				return err
			})
		}
		if errors.Is(err, store.ErrNotFound) {
			err = write("")
		} else if err == nil {
			err = s.app.WithProjectConfiguration(userID, project.ID, func() error { return write(project.ID) })
		}
		if errors.Is(err, store.ErrProjectSyncIdentityChanged) || errors.Is(err, app.ErrNotFound) {
			continue
		}
		if errors.Is(err, app.ErrNotAllowed) {
			err = store.ErrProjectSyncDenied
		}
		return result, err
	}
	return store.ProjectSyncResult{}, store.ErrProjectSyncIdentityChanged
}

func readProjectSync(w http.ResponseWriter, r *http.Request) (store.ProjectDeclaration, error) {
	var declaration store.ProjectDeclaration
	invalid := errors.New("invalid project declaration")
	control := http.NewResponseController(w)
	_ = control.SetReadDeadline(time.Now().Add(15 * time.Second))
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, projectSyncBodyLimit+pipeline.MaxWorkflowBodyBytes))
	_ = control.SetReadDeadline(time.Time{})
	if err != nil || !utf8.Valid(data) {
		return declaration, invalid
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := uniqueSyncJSON(decoder); err != nil {
		return declaration, invalid
	}
	if _, err := decoder.Token(); err != io.EOF {
		return declaration, invalid
	}
	body, err := syncObject(data, map[string]bool{"name": true, "repository": true, "description": true, "capacity": true, "workflows": true})
	if err != nil {
		return declaration, invalid
	}
	workflowBytes := 0
	if raw, ok := body["workflows"]; ok {
		workflowBytes = len(raw)
		request, err := http.NewRequest(http.MethodPut, "/", bytes.NewReader(append(append([]byte(`{"workflows":`), raw...), '}')))
		if err != nil {
			return declaration, invalid
		}
		declaration.Workflows, err = readWorkflows(w, request)
		if err != nil {
			return declaration, invalid
		}
		delete(body, "workflows")
	}
	if len(data)-workflowBytes > projectSyncBodyLimit {
		return declaration, invalid
	}
	if !syncString(body["name"], &declaration.Name) || strings.TrimSpace(declaration.Name) == "" || !syncString(body["repository"], &declaration.Repository) {
		return declaration, invalid
	}
	if declaration.Repository != "" {
		canonical, err := gitorigin.Canonical(declaration.Repository)
		if err != nil {
			return declaration, invalid
		}
		declaration.Repository = canonical
	}
	if raw, ok := body["description"]; ok {
		var description string
		if !syncString(raw, &description) {
			return declaration, invalid
		}
		declaration.Description = &description
	}
	if raw, ok := body["capacity"]; ok {
		fields, err := syncObject(raw, map[string]bool{"worker_selector": true, "max_swarms": true, "max_actors": true, "max_cpu_millicores": true, "max_memory_bytes": true})
		if err != nil {
			return declaration, invalid
		}
		if selector, ok := fields["worker_selector"]; ok {
			var labels map[string]json.RawMessage
			if len(selector) == 0 || selector[0] != '{' || json.Unmarshal(selector, &labels) != nil {
				return declaration, invalid
			}
			for _, value := range labels {
				var label string
				if !syncString(value, &label) {
					return declaration, invalid
				}
			}
		}
		for key, value := range fields {
			if key == "worker_selector" || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				continue
			}
			var limit string
			if !syncString(value, &limit) {
				return declaration, invalid
			}
		}
		var wire capacityJSON
		if json.Unmarshal(raw, &wire) != nil {
			return declaration, invalid
		}
		capacity, err := parseCapacity(wire)
		if err != nil {
			return declaration, invalid
		}
		stored := store.ProjectCapacity{WorkerSelector: capacity.WorkerSelector, MaxSwarms: capacity.MaxSwarms, MaxActors: capacity.MaxActors, MaxCPUMillicores: capacity.MaxCPUMillicores, MaxMemoryBytes: capacity.MaxMemoryBytes}
		if err := store.ValidateProjectCapacity(stored); err != nil {
			return declaration, invalid
		}
		declaration.Capacity = &stored
	}
	return declaration, nil
}

func syncString(raw json.RawMessage, target *string) bool {
	return len(raw) > 0 && raw[0] == '"' && json.Unmarshal(raw, target) == nil
}

func syncObject(raw []byte, allowed map[string]bool) (map[string]json.RawMessage, error) {
	var result map[string]json.RawMessage
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' || json.Unmarshal(raw, &result) != nil || result == nil {
		return nil, errors.New("object required")
	}
	for key := range result {
		if !allowed[key] {
			return nil, errors.New("unknown field")
		}
	}
	return result, nil
}

// uniqueSyncJSON отвергает повторы на любой глубине до декодирования структур.
func uniqueSyncJSON(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	if delimiter != '{' && delimiter != '[' {
		return errors.New("invalid JSON")
	}
	keys := map[string]bool{}
	for d.More() {
		if delimiter == '{' {
			token, err := d.Token()
			if err != nil {
				return err
			}
			key, ok := token.(string)
			if !ok || keys[key] {
				return errors.New("duplicate JSON field")
			}
			keys[key] = true
		}
		if err := uniqueSyncJSON(d); err != nil {
			return err
		}
	}
	end, err := d.Token()
	if err != nil {
		return err
	}
	if delimiter == '{' && end != json.Delim('}') || delimiter == '[' && end != json.Delim(']') {
		return errors.New("invalid JSON")
	}
	return nil
}
