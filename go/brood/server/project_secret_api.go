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
	"brood.dev/go/brood/auth"
	"brood.dev/go/brood/projectsecret"
	"brood.dev/go/brood/store"
)

const projectSecretBodyLimit = 4 << 20

func (s *Server) mountProjectSecrets() {
	s.mux.HandleFunc("GET /api/projects/{id}/secrets", s.handleProjectSecretMetadata)
	for _, kind := range []string{"files", "vars"} {
		s.mux.HandleFunc("GET /api/projects/{id}/secrets/"+kind, func(w http.ResponseWriter, r *http.Request) {
			if _, ok := s.projectSecretAdmin(w, r); !ok {
				return
			}
			w.Header().Set("Allow", "PUT, DELETE")
			writeDetail(w, 405, "secret values are not readable through this API")
		})
		s.mux.HandleFunc("PUT /api/projects/{id}/secrets/"+kind, s.handleProjectSecretMutation)
		s.mux.HandleFunc("DELETE /api/projects/{id}/secrets/"+kind, s.handleProjectSecretMutation)
	}
}

func (s *Server) projectSecretAdmin(w http.ResponseWriter, r *http.Request) (string, bool) {
	user, ok := currentUser(w, r)
	if !ok {
		return "", false
	}
	current, err := s.store.GetUser(r.Context(), user.ID)
	if err != nil || current.Disabled || !current.IsAdmin {
		writeDetail(w, 403, "administrator access required for project secrets")
		return "", false
	}
	if _, err := s.store.GetProject(r.Context(), r.PathValue("id")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeDetail(w, 404, "project not found")
		} else {
			writeDetail(w, 503, "project secret service unavailable")
		}
		return "", false
	}
	w.Header().Set("Cache-Control", "no-store")
	return user.ID, true
}

func (s *Server) handleProjectSecretMetadata(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.projectSecretAdmin(w, r); !ok {
		return
	}
	metadata, err := s.store.GetProjectSecretMetadata(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDetail(w, 503, "project secret metadata unavailable")
		return
	}
	if !s.validProxySession(r) {
		writeDetail(w, 403, "secret session authorization changed")
		return
	}
	writeJSON(w, 200, metadata)
}

func secretJSONValue(d *json.Decoder, depth int, tokens *int, field string) bool {
	*tokens++
	if depth > 16 || *tokens > 32768 {
		return false
	}
	value, err := d.Token()
	if err != nil || value == nil {
		return false
	}
	delimiter, ok := value.(json.Delim)
	if !ok {
		return true
	}
	switch delimiter {
	case '{':
		keys := map[string]bool{}
		for d.More() {
			token, err := d.Token()
			key, ok := token.(string)
			if err != nil || !ok || keys[key] {
				return false
			}
			if depth == 0 && key != "vars" && key != "files" && key != "names" {
				return false
			}
			if depth == 2 && field == "files" && key != "name" && key != "data" {
				return false
			}
			keys[key] = true
			if !secretJSONValue(d, depth+1, tokens, key) {
				return false
			}
		}
		end, err := d.Token()
		return err == nil && end == json.Delim('}')
	case '[':
		for d.More() {
			if !secretJSONValue(d, depth+1, tokens, field) {
				return false
			}
		}
		end, err := d.Token()
		return err == nil && end == json.Delim(']')
	}
	return false
}

func readProjectSecretBody(w http.ResponseWriter, r *http.Request, out any) bool {
	control := http.NewResponseController(w)
	_ = control.SetReadDeadline(time.Now().Add(15 * time.Second))
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, projectSecretBodyLimit))
	_ = control.SetReadDeadline(time.Time{})
	if err != nil || !utf8.Valid(data) || len(bytes.TrimSpace(data)) == 0 || bytes.TrimSpace(data)[0] != '{' {
		writeDetail(w, 400, "invalid or oversized secret request")
		return false
	}
	scan := json.NewDecoder(bytes.NewReader(data))
	tokens := 0
	if !secretJSONValue(scan, 0, &tokens, "") || scan.Decode(new(any)) != io.EOF {
		writeDetail(w, 400, "invalid or duplicate secret request fields")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(out) != nil || decoder.Decode(new(any)) != io.EOF {
		writeDetail(w, 400, "invalid secret request fields")
		return false
	}
	return true
}

func secretRequestSession(ctx context.Context, s *Server, r *http.Request, userID string) bool {
	cookie, err := r.Cookie(auth.CookieSession)
	if err != nil {
		return false
	}
	id, _, err := auth.NewSessions(s.store, 0).Load(ctx, cookie.Value, time.Now())
	return err == nil && id == userID
}

func (s *Server) handleProjectSecretMutation(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.projectSecretAdmin(w, r)
	if !ok {
		return
	}
	kind := "vars"
	if strings.HasSuffix(r.URL.Path, "/files") {
		kind = "files"
	}
	var apply func(context.Context) error
	if r.Method == http.MethodDelete {
		var request struct {
			Names []string `json:"names"`
		}
		if !readProjectSecretBody(w, r, &request) {
			return
		}
		if len(request.Names) == 0 {
			writeDetail(w, 400, "secret names are required")
			return
		}
		apply = func(ctx context.Context) error {
			if kind == "files" {
				return s.store.RemoveProjectSecretFiles(ctx, r.PathValue("id"), request.Names)
			}
			return s.store.RemoveProjectSecretVars(ctx, r.PathValue("id"), request.Names)
		}
	} else if kind == "files" {
		var request struct {
			Files []struct {
				Name string  `json:"name"`
				Data *[]byte `json:"data"`
			} `json:"files"`
		}
		if !readProjectSecretBody(w, r, &request) {
			return
		}
		files := make([]projectsecret.File, 0, len(request.Files))
		for _, file := range request.Files {
			if file.Data == nil {
				writeDetail(w, 400, "secret file data is required")
				return
			}
			files = append(files, projectsecret.File{Name: file.Name, Data: *file.Data})
		}
		if len(files) == 0 || projectsecret.ValidateBatch(files, nil) != nil {
			writeDetail(w, 400, "invalid secret file batch or limits")
			return
		}
		apply = func(ctx context.Context) error { return s.store.PutProjectSecretFiles(ctx, r.PathValue("id"), files) }
	} else {
		var request struct {
			Vars map[string]string `json:"vars"`
		}
		if !readProjectSecretBody(w, r, &request) {
			return
		}
		if len(request.Vars) == 0 || projectsecret.ValidateBatch(nil, request.Vars) != nil {
			writeDetail(w, 400, "invalid secret variable batch or reserved environment")
			return
		}
		apply = func(ctx context.Context) error {
			return s.store.PutProjectSecretVars(ctx, r.PathValue("id"), request.Vars)
		}
	}
	err := s.store.WithTx(r.Context(), func(ctx context.Context) error {
		if !secretRequestSession(ctx, s, r, userID) {
			return app.ErrNotAllowed
		}
		user, err := s.store.GetUser(ctx, userID)
		if err != nil || user.Disabled || !user.IsAdmin {
			return app.ErrNotAllowed
		}
		return apply(ctx)
	})
	if errors.Is(err, app.ErrNotAllowed) {
		writeDetail(w, 403, "project secret authorization changed")
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		writeDetail(w, 404, "project not found")
		return
	}
	if err != nil {
		writeDetail(w, 400, "secret request rejected; verify names, sizes and project limits")
		return
	}
	metadata, err := s.store.GetProjectSecretMetadata(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDetail(w, 503, "secret mutation outcome requires verification")
		return
	}
	if !s.validProxySession(r) {
		writeDetail(w, 403, "secret session authorization changed")
		return
	}
	writeJSON(w, 200, metadata)
}
