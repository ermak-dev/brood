package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/store"
)

var recoveryIDPattern = regexp.MustCompile(`^(?:[0-7][0123456789ABCDEFGHJKMNPQRSTVWXYZ]{25}|[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12})$`)
var recoveryNamespacePattern = regexp.MustCompile(`^swarm-(?:[0-7][0123456789abcdefghjkmnpqrstvwxyz]{25}|[0-9a-f]{32})$`)

type orphanCandidate struct {
	AtespaceName string `json:"atespace_name"`
	AtespaceUID  string `json:"atespace_uid"`
}

type orphanRecoveryRequest struct {
	ProjectID   string `json:"project_id"`
	SwarmID     string `json:"swarm_id"`
	AtespaceUID string `json:"atespace_uid"`
}

func (s *Server) mountOrphanRecovery() {
	s.mux.HandleFunc("GET /api/admin/orphan-swarms", s.handleOrphanCandidates)
	s.mux.HandleFunc("GET /api/admin/orphan-swarms/{candidate}", s.handleOrphanInspection)
	s.mux.HandleFunc("POST /api/admin/orphan-swarms/recover", s.handleOrphanRecovery)
}

func recoveryAdmin(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	user, ok := currentUser(w, r)
	if !ok {
		return user, false
	}
	if !user.IsAdmin {
		writeDetail(w, http.StatusForbidden, "administrator access required")
		return store.User{}, false
	}
	return user, true
}

func (s *Server) handleOrphanCandidates(w http.ResponseWriter, r *http.Request) {
	if _, ok := recoveryAdmin(w, r); !ok {
		return
	}
	client, ok := s.clientOr503(w, r)
	if !ok {
		return
	}
	spaces, err := client.ListAtespaces(r.Context())
	if err != nil {
		apiError(w, r, err)
		return
	}
	items := []orphanCandidate{}
	for _, space := range spaces {
		name, uid := space.GetMetadata().GetName(), space.GetMetadata().GetUid()
		if !recoveryNamespacePattern.MatchString(name) || uid == "" || space.GetMetadata().GetDeleteTime() != nil {
			continue
		}
		_, err := s.store.GetSwarmByAtespace(r.Context(), name)
		if err == nil {
			continue
		}
		if !errors.Is(err, store.ErrNotFound) {
			apiError(w, r, err)
			return
		}
		items = append(items, orphanCandidate{AtespaceName: name, AtespaceUID: uid})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].AtespaceName < items[j].AtespaceName })
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func readOrphanRecovery(w http.ResponseWriter, r *http.Request) (orphanRecoveryRequest, bool) {
	var body orphanRecoveryRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeDetail(w, http.StatusBadRequest, "invalid recovery request; use project_id, swarm_id and atespace_uid as strings")
		return body, false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeDetail(w, http.StatusBadRequest, "recovery request must contain a single JSON object")
		return body, false
	}
	if !recoveryIDPattern.MatchString(body.SwarmID) {
		writeDetail(w, http.StatusBadRequest, "swarm_id must be a canonical uppercase ULID or lowercase UUID")
		return body, false
	}
	if strings.TrimSpace(body.ProjectID) == "" || strings.TrimSpace(body.AtespaceUID) == "" {
		writeDetail(w, http.StatusBadRequest, "project_id and atespace_uid are required")
		return body, false
	}
	return body, true
}

func (s *Server) recoveryProject(ctx context.Context, projectID string) error {
	_, err := s.store.GetProject(ctx, projectID)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%w: project not found", app.ErrNotFound)
	}
	return err
}

func (s *Server) requireUntracked(ctx context.Context, swarmID, atespace string) error {
	_, err := s.store.GetSwarm(ctx, swarmID)
	if err == nil {
		return fmt.Errorf("%w: swarm is already tracked", app.ErrConflict)
	}
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	_, err = s.store.GetSwarmByAtespace(ctx, atespace)
	if err == nil {
		return fmt.Errorf("%w: namespace is already tracked", app.ErrConflict)
	}
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}

func (s *Server) handleOrphanRecovery(w http.ResponseWriter, r *http.Request) {
	user, ok := recoveryAdmin(w, r)
	if !ok {
		return
	}
	body, ok := readOrphanRecovery(w, r)
	if !ok {
		return
	}
	name := app.AtespaceName(body.SwarmID)
	if err := s.recoveryProject(r.Context(), body.ProjectID); err != nil {
		apiError(w, r, err)
		return
	}
	if err := s.requireUntracked(r.Context(), body.SwarmID, name); err != nil {
		apiError(w, r, err)
		return
	}
	client, ok := s.clientOr503(w, r)
	if !ok {
		return
	}
	space, err := client.GetAtespace(r.Context(), name)
	if err != nil {
		apiError(w, r, err)
		return
	}
	if space.GetMetadata().GetName() != name || space.GetMetadata().GetUid() != body.AtespaceUID {
		writeDetail(w, http.StatusConflict, "namespace UID or name does not match the expected identity")
		return
	}
	if space.GetMetadata().GetDeleteTime() != nil {
		writeDetail(w, http.StatusConflict, "namespace is terminating; wait for native deletion")
		return
	}
	var saved store.Swarm
	var ownerUsername string
	err = s.store.WithTx(r.Context(), func(ctx context.Context) error {
		actor, err := s.store.GetUser(ctx, user.ID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && (actor.Disabled || !actor.IsAdmin)) {
			return app.ErrNotAllowed
		}
		if err != nil {
			return err
		}
		if err := s.recoveryProject(ctx, body.ProjectID); err != nil {
			return err
		}
		if err := s.requireUntracked(ctx, body.SwarmID, name); err != nil {
			return err
		}
		swarm := store.Swarm{ID: body.SwarmID, ProjectID: body.ProjectID, OwnerID: actor.ID,
			Name: "recovered-" + strings.ToLower(body.SwarmID), AtespaceName: name,
			AtespaceUID: &body.AtespaceUID, State: string(app.StateActive)}
		if err := s.store.CreateSwarm(ctx, swarm); err != nil {
			if store.IsUniqueViolation(err) {
				return fmt.Errorf("%w: swarm identity or name is already tracked", app.ErrConflict)
			}
			return err
		}
		saved, err = s.store.GetSwarm(ctx, swarm.ID)
		ownerUsername = actor.Username
		return err
	})
	if err != nil {
		apiError(w, r, err)
		return
	}
	row := toSwarmJSON(toAppSwarm(saved), ownerUsername)
	row.Role = app.RoleOwner
	writeJSON(w, http.StatusCreated, row)
}
