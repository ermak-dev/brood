package server

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/store"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
)

type orphanActorInfo struct {
	Name         string `json:"name"`
	UID          string `json:"uid"`
	TemplateName string `json:"template_name"`
}

type orphanTemplateInfo struct {
	Name   string   `json:"name"`
	UID    string   `json:"uid"`
	Images []string `json:"images"`
}

func inspectionNamespace(space *ateapi.Atespace, name, uid string) error {
	if space.GetMetadata().GetName() != name || space.GetMetadata().GetUid() != uid {
		return fmt.Errorf("%w: namespace UID or name does not match the expected identity", app.ErrConflict)
	}
	if space.GetMetadata().GetDeleteTime() != nil {
		return fmt.Errorf("%w: namespace is terminating", app.ErrConflict)
	}
	return nil
}

func (s *Server) handleOrphanInspection(w http.ResponseWriter, r *http.Request) {
	user, ok := recoveryAdmin(w, r)
	if !ok {
		return
	}
	id, uid := r.PathValue("candidate"), r.URL.Query().Get("atespace_uid")
	if !recoveryIDPattern.MatchString(id) || strings.TrimSpace(uid) == "" {
		writeDetail(w, 400, "use a canonical uppercase ULID or lowercase UUID and the expected atespace_uid")
		return
	}
	name := app.AtespaceName(id)
	if err := s.requireUntracked(r.Context(), id, name); err != nil {
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
	if err := inspectionNamespace(space, name, uid); err != nil {
		apiError(w, r, err)
		return
	}
	actors, err := client.ListActors(r.Context(), name)
	if err != nil {
		apiError(w, r, err)
		return
	}
	templates, err := client.ListActorTemplates(r.Context(), name)
	if err != nil {
		apiError(w, r, err)
		return
	}
	actorInfo := make([]orphanActorInfo, 0, len(actors))
	for _, actor := range actors {
		if actor.GetMetadata().GetAtespace() != name {
			writeDetail(w, 409, "actor inventory does not match the expected namespace")
			return
		}
		actorInfo = append(actorInfo, orphanActorInfo{Name: actor.GetMetadata().GetName(), UID: actor.GetMetadata().GetUid(), TemplateName: actor.GetActorTemplate().GetName()})
	}
	templateInfo := make([]orphanTemplateInfo, 0, len(templates))
	for _, template := range templates {
		if template.GetMetadata().GetAtespace() != name {
			writeDetail(w, 409, "template inventory does not match the expected namespace")
			return
		}
		images := make([]string, 0, len(template.GetContainers()))
		for _, container := range template.GetContainers() {
			images = append(images, container.GetImage())
		}
		templateInfo = append(templateInfo, orphanTemplateInfo{Name: template.GetMetadata().GetName(), UID: template.GetMetadata().GetUid(), Images: images})
	}
	space, err = client.GetAtespace(r.Context(), name)
	if err != nil {
		apiError(w, r, err)
		return
	}
	if err := inspectionNamespace(space, name, uid); err != nil {
		apiError(w, r, err)
		return
	}
	current, err := s.store.GetUser(r.Context(), user.ID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && (current.Disabled || !current.IsAdmin)) {
		writeDetail(w, 403, "administrator access required")
		return
	}
	if err != nil {
		apiError(w, r, err)
		return
	}
	if err := s.requireUntracked(r.Context(), id, name); err != nil {
		apiError(w, r, err)
		return
	}
	sort.Slice(actorInfo, func(i, j int) bool { return actorInfo[i].Name < actorInfo[j].Name })
	sort.Slice(templateInfo, func(i, j int) bool { return templateInfo[i].Name < templateInfo[j].Name })
	writeJSON(w, 200, struct {
		AtespaceName string               `json:"atespace_name"`
		AtespaceUID  string               `json:"atespace_uid"`
		Actors       []orphanActorInfo    `json:"actors"`
		Templates    []orphanTemplateInfo `json:"templates"`
	}{name, uid, actorInfo, templateInfo})
}
