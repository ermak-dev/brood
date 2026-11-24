// Агент в HTTP API: тело создания и объект, который возвращают
// /api/agents и /api/swarms/{id}/agents.
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

// agentInput is the body that creates an agent. The pool sets everything
// else; the client does not override it. An empty RoleID takes the default
// role, an empty Origin the repository of the project.
type agentInput struct {
	Name   string `json:"name"`
	Pool   string `json:"pool"`
	RoleID string `json:"roleId"`
	Origin string `json:"origin"`
}

// readAgentInput разбирает тело создания агента; брак — 400, ответ записан.
func readAgentInput(w http.ResponseWriter, r *http.Request) (agentInput, bool) {
	var input agentInput
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxResourceBody))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&input)
	if err == nil && decoder.Decode(&struct{}{}) != io.EOF {
		err = errors.New("trailing data")
	}
	var typed *json.UnmarshalTypeError
	switch {
	case err == nil:
		return input, true
	case errors.As(err, &typed) && typed.Field != "":
		writeDetail(w, http.StatusBadRequest, typed.Field+" must be a string; no agent was created")
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		field := strings.TrimPrefix(err.Error(), "json: unknown field ")
		writeDetail(w, http.StatusBadRequest, "unknown agent field "+field+"; no agent was created. An agent takes name, pool, roleId and origin")
	default:
		writeDetail(w, http.StatusBadRequest, "malformed request body; no agent was created")
	}
	return agentInput{}, false
}

// agentJSON is an agent as the HTTP API shows it. Phase is the state of the
// container as a person sees it; Desired is the state the server drives it
// to. Work is set for a running agent only.
type agentJSON struct {
	Name          string     `json:"name"`
	UID           string     `json:"uid"`
	Swarm         string     `json:"swarm"`
	SwarmName     string     `json:"swarmName"`
	ProjectID     string     `json:"projectId"`
	ProjectName   string     `json:"projectName"`
	Role          string     `json:"role"`
	Phase         string     `json:"phase"`
	Desired       string     `json:"desired"`
	Generation    int64      `json:"generation"`
	Error         string     `json:"error"`
	ErrorCode     string     `json:"errorCode"`
	Pool          string     `json:"pool"`
	Node          string     `json:"node"`
	CTID          int        `json:"ctid"`
	Profile       string     `json:"profile"`
	Origin        string     `json:"origin"`
	CPUMillicores int64      `json:"cpuMillicores"`
	MemoryBytes   int64      `json:"memoryBytes"`
	DiskBytes     int64      `json:"diskBytes"`
	CreatedAt     string     `json:"createdAt"`
	Work          *agentWork `json:"work,omitempty"`
}

// agentPhase — фаза агента, какой её видит человек. Stop, ещё не
// запрошенный у PVE (#275), — та же остановка, что stopping или deleting:
// CT работает лишь до запроса. Незапрошенный Delete (#277) — то же
// удаление, что запрошенный. Хранимая phase остаётся наблюдением CT.
func agentPhase(a store.ManagedActor) string {
	switch {
	case stopNotRequested(a):
		return stopPhase(a)
	case deleteNotRequested(a):
		return deletePhase(a)
	}
	return a.Phase
}

// agentView строит объект агента; project — имя проекта роя.
func (s *Server) agentView(ctx context.Context, sw app.Swarm, project string, a store.ManagedActor, p store.ManagedPool) agentJSON {
	diagnostic := a.Diagnostic()
	return agentJSON{
		Name: a.Name, UID: a.UID, Swarm: sw.ID, SwarmName: sw.Name, ProjectID: sw.ProjectID, ProjectName: project,
		Role: s.agentRoleName(ctx, a.Role), Phase: agentPhase(a), Desired: a.Desired, Generation: a.Incarnation,
		Error: diagnostic, ErrorCode: managedErrorCode(diagnostic),
		Pool: p.Name, Node: p.Source.Node, CTID: a.CTID, Profile: a.Template.Profile, Origin: a.Origin,
		CPUMillicores: a.Resources.CPUMillicores, MemoryBytes: a.Resources.MemoryBytes, DiskBytes: a.Resources.DiskBytes,
		CreatedAt: a.CreatedAt,
	}
}

// projectName — имя проекта роя; пусто, если проект не прочитан.
func (s *Server) projectName(ctx context.Context, sw app.Swarm) string {
	project, err := s.store.GetProject(ctx, sw.ProjectID)
	if err != nil {
		return ""
	}
	return project.Name
}

// writeAgent отвечает одним агентом роя.
func (s *Server) writeAgent(w http.ResponseWriter, r *http.Request, status int, sw app.Swarm, a store.ManagedActor, p store.ManagedPool) {
	writeJSON(w, status, s.agentView(r.Context(), sw, s.projectName(r.Context(), sw), a, p))
}

// agentsJSON — живые агенты роя с работой тех, что запущены.
func (s *Server) agentsJSON(ctx context.Context, sw app.Swarm, project string) ([]agentJSON, error) {
	actors, err := s.store.ListManagedActors(ctx, sw.ID)
	if err != nil {
		return nil, err
	}
	works := s.managed.agentWorks(ctx, actors, false)
	out := []agentJSON{}
	for _, a := range actors {
		if a.Phase == "deleted" {
			continue
		}
		p, err := s.store.GetManagedPool(ctx, a.PoolUID)
		if err != nil {
			return nil, err
		}
		row := s.agentView(ctx, sw, project, a, p)
		if work, ok := works[a.UID]; ok {
			row.Work = &work
		}
		out = append(out, row)
	}
	return out, nil
}
