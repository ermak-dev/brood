package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/auth"
	"brood.dev/go/brood/pipeline"
	"brood.dev/go/brood/piprender"
	"brood.dev/go/brood/store"
)

// pipelineNodeJSON — шаг роя: посещение узла. Sequence, StartedAt и
// FinishedAt — номер шага и его записанные моменты (#206), без них
// страница роя не может показать, когда шаг шёл (#215).
type pipelineNodeJSON struct {
	ID         string  `json:"id"`
	Sequence   int64   `json:"sequence"`
	Kind       string  `json:"kind"`
	Status     string  `json:"status"`
	Label      string  `json:"label"`
	Outcome    string  `json:"outcome,omitempty"`
	ActorName  string  `json:"actorName,omitempty"`
	StartedAt  *string `json:"startedAt,omitempty"`
	FinishedAt *string `json:"finishedAt,omitempty"`
}

// pipelineGraphNodeJSON — узел графа роя с тем, что граф ему поручает:
// prompt актору или вопрос оператору. Страница роя показывает это у
// выбранного узла (#215), в том числе у ещё не посещённого.
type pipelineGraphNodeJSON struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Prompt   string `json:"prompt,omitempty"`
	Question string `json:"question,omitempty"`
}
type pipelineOptionJSON struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}
type pipelineGateJSON struct {
	RunID    string               `json:"runId"`
	NodeID   string               `json:"nodeId"`
	Sequence int64                `json:"visitSequence"`
	Question string               `json:"question"`
	Kind     string               `json:"kind"`
	Options  []pipelineOptionJSON `json:"options"`
}
type pipelineRunJSON struct {
	ID          string             `json:"id"`
	Name        string             `json:"name"`
	Status      string             `json:"status"`
	SwarmID     string             `json:"swarmId"`
	SwarmName   string             `json:"swarmName"`
	ProjectID   string             `json:"projectId"`
	ProjectName string             `json:"projectName"`
	Owner       string             `json:"owner"`
	StartedAt   *string            `json:"startedAt"`
	FinishedAt  *string            `json:"finishedAt,omitempty"`
	CurrentNode string             `json:"currentNode,omitempty"`
	Nodes       []pipelineNodeJSON `json:"nodes"`
	Gate        *pipelineGateJSON  `json:"gate,omitempty"`
}

// legacyRunStatus переводит состояние исполнения роя (#206) в статус
// прогона старого провода: маршруты /api/pipeline-runs остаются до
// полной замены (#210/#211), но читают рой.
func legacyRunStatus(execution string) string {
	switch execution {
	case "waiting":
		return "waiting_human"
	default:
		return execution
	}
}

func (s *Server) mountPipelineAPI() {
	s.mux.HandleFunc("GET /api/swarms/{id}/pipeline-runs", s.pipelineList)
	s.mux.HandleFunc("GET /api/pipeline-runs/{run}", s.pipelineDetail)
	s.mux.HandleFunc("GET /api/pipeline-runs/{run}/graph", s.pipelineGraph)
	s.mux.HandleFunc("POST /api/pipeline-runs/{run}/nodes/{node}/answer", s.pipelineAnswer)
}

// pipelineList отдаёт рои проекта как список «прогонов»: один рой с
// графом — один прогон, рой без графа не попадает в список. Список
// ограничен проектом роя из пути: чужой проект не виден.
func (s *Server) pipelineList(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	allowed, err := s.app.Can(user.ID, app.ActionViewSwarm, app.SwarmRes(id))
	if err != nil {
		apiError(w, r, err)
		return
	}
	if !allowed {
		writeDetail(w, 404, "swarm not found")
		return
	}
	target, err := s.store.GetSwarm(r.Context(), id)
	if err != nil {
		apiError(w, r, err)
		return
	}
	swarms, err := s.store.ListSwarmsByProject(r.Context(), target.ProjectID)
	if err != nil {
		apiError(w, r, err)
		return
	}
	manage, err := s.app.Can(user.ID, app.ActionManageSwarm, app.SwarmRes(id))
	if err != nil {
		apiError(w, r, err)
		return
	}
	out := []pipelineRunJSON{}
	for _, sw := range swarms {
		if sw.Graph == nil {
			continue
		}
		// Рой без права чтения для этого пользователя в список не попадает.
		visible, err := s.app.Can(user.ID, app.ActionViewSwarm, app.SwarmRes(sw.ID))
		if err != nil {
			apiError(w, r, err)
			return
		}
		if !visible {
			continue
		}
		steps, err := s.store.SwarmSteps(r.Context(), sw.ID)
		if err != nil {
			apiError(w, r, err)
			return
		}
		v, err := s.pipelineView(r.Context(), sw, steps, manage)
		if err != nil {
			apiError(w, r, err)
			return
		}
		out = append(out, v)
	}
	writeJSON(w, 200, out)
}

// pipelineFind ищет рой-«прогон» по ID: до замены маршрутов (#210/#211)
// {run} — это ID роя. Список роёв проходится явно: store намеренно не
// даёт глобального чтения по произвольному ID без роя.
func (s *Server) pipelineFind(w http.ResponseWriter, r *http.Request, action app.Action) (store.Swarm, bool) {
	user, ok := currentUser(w, r)
	if !ok {
		return store.Swarm{}, false
	}
	id := r.PathValue("run")
	target, err := s.store.GetSwarm(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || err == nil && target.Graph == nil {
		writeDetail(w, 404, "pipeline run not found")
		return store.Swarm{}, false
	}
	if err != nil {
		apiError(w, r, err)
		return store.Swarm{}, false
	}
	allowed, err := s.app.Can(user.ID, action, app.SwarmRes(target.ID))
	if err != nil {
		apiError(w, r, err)
		return store.Swarm{}, false
	}
	if !allowed {
		writeDetail(w, 404, "pipeline run not found")
		return store.Swarm{}, false
	}
	return target, true
}

// pipelineRunView renders a swarm for the request user.
func (s *Server) pipelineRunView(r *http.Request, sw store.Swarm) (pipelineRunJSON, error) {
	steps, err := s.store.SwarmSteps(r.Context(), sw.ID)
	if err != nil {
		return pipelineRunJSON{}, err
	}
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		return pipelineRunJSON{}, errors.New("authentication required")
	}
	manage, err := s.app.Can(user.ID, app.ActionManageSwarm, app.SwarmRes(sw.ID))
	if err != nil {
		return pipelineRunJSON{}, err
	}
	return s.pipelineView(r.Context(), sw, steps, manage)
}

// pipelineView renders a swarm graph and its steps; manage offers the
// waiting human gate. startedAt/finishedAt идут из реальных моментов
// начала и завершения шагов (#206).
func (s *Server) pipelineView(ctx context.Context, sw store.Swarm, steps []store.PipelineVisit, manage bool) (pipelineRunJSON, error) {
	d, err := pipeline.Parse(string(sw.Graph), "pipeline")
	if err != nil {
		return pipelineRunJSON{}, err
	}
	project, err := s.store.GetProject(ctx, sw.ProjectID)
	if err != nil {
		return pipelineRunJSON{}, err
	}
	owner, err := s.store.GetUser(ctx, sw.OwnerID)
	if err != nil {
		return pipelineRunJSON{}, err
	}
	result := pipelineRunJSON{ID: sw.ID, Name: d.Name, Status: legacyRunStatus(sw.Execution), SwarmID: sw.ID, SwarmName: sw.Name, ProjectID: project.ID, ProjectName: project.Name, Owner: owner.Username, Nodes: []pipelineNodeJSON{}}
	for _, v := range steps {
		n := d.Nodes[v.NodeID]
		row := pipelineNodeJSON{ID: v.NodeID, Sequence: v.Sequence, Kind: n.Type, Label: v.NodeID, Status: v.State, ActorName: v.ActorName, StartedAt: v.StartedAt, FinishedAt: v.FinishedAt}
		switch v.State {
		case "waiting", "inflight":
			row.Status = "active"
		case "answered":
			row.Status = "done"
		case "completed":
			if v.Outcome != nil && *v.Outcome == "success" || v.NodeID == d.Exit && (v.Outcome == nil || *v.Outcome == "") {
				row.Status = "done"
			} else if v.Outcome != nil && *v.Outcome == "fail" {
				row.Status = "failed"
			}
		}
		if v.Outcome != nil {
			row.Outcome = *v.Outcome
		} else if v.Answer != nil {
			row.Outcome = *v.Answer
		}
		result.Nodes = append(result.Nodes, row)
	}
	if len(steps) > 0 {
		v := steps[len(steps)-1]
		result.CurrentNode = v.NodeID
		if v.StartedAt != nil {
			result.StartedAt = v.StartedAt
		}
		result.FinishedAt = lastFinishedAt(steps)
		n := d.Nodes[v.NodeID]
		open, err := s.gateRequestOpen(ctx, sw.ID, v)
		if err != nil {
			return pipelineRunJSON{}, err
		}
		if manage && open && n.Type == "human" && activeExecutionView(sw.Execution) {
			gate := &pipelineGateJSON{RunID: sw.ID, NodeID: v.NodeID, Sequence: v.Sequence, Question: n.Question, Kind: "multiple_choice", Options: []pipelineOptionJSON{}}
			for _, o := range n.Options {
				gate.Options = append(gate.Options, pipelineOptionJSON{ID: o.Outcome, Label: o.Label})
			}
			result.Gate = gate
		}
	}
	return result, nil
}

// gateRequestOpen: шлюз предлагается к ответу только при открытом узловом
// запросе; отменённый запрос не выдаётся за активный.
func (s *Server) gateRequestOpen(ctx context.Context, swarmID string, v store.PipelineVisit) (bool, error) {
	if v.State != "waiting" {
		return false, nil
	}
	requests, err := s.store.SwarmRequests(ctx, swarmID)
	if err != nil {
		return false, err
	}
	for _, req := range requests {
		if req.Origin == "node" && req.Sequence != nil && *req.Sequence == v.Sequence {
			return req.State == "open", nil
		}
	}
	return false, nil
}

// activeExecutionView — прикладной взгляд сервера на активность графа.
func activeExecutionView(execution string) bool {
	return execution == "running" || execution == "waiting"
}

// lastFinishedAt возвращает момент завершения последнего завершённого
// шага: время окончания прогона.
func lastFinishedAt(steps []store.PipelineVisit) *string {
	for i := len(steps) - 1; i >= 0; i-- {
		if steps[i].FinishedAt != nil {
			return steps[i].FinishedAt
		}
	}
	return nil
}

func (s *Server) pipelineGraph(w http.ResponseWriter, r *http.Request) {
	sw, ok := s.pipelineFind(w, r, app.ActionViewSwarm)
	if !ok {
		return
	}
	d, err := pipeline.Parse(string(sw.Graph), "pipeline")
	if err != nil {
		apiError(w, r, err)
		return
	}
	steps, err := s.store.SwarmSteps(r.Context(), sw.ID)
	if err != nil {
		apiError(w, r, err)
		return
	}
	view, err := s.pipelineView(r.Context(), sw, steps, false)
	if err != nil {
		apiError(w, r, err)
		return
	}
	g := d.Graph()
	states := map[string]piprender.State{}
	for _, n := range view.Nodes {
		states[n.ID] = piprender.State(n.Status)
	}
	svg := piprender.SVG(piprender.RunView{Graph: g, Nodes: states})
	writeJSON(w, 200, struct {
		SVG   string                  `json:"svg"`
		Nodes []pipelineGraphNodeJSON `json:"nodes"`
	}{svg, graphNodes(d)})
}

// graphNodes перечисляет узлы снимка графа в порядке ID: порядок ответа
// не зависит от обхода map.
func graphNodes(d *pipeline.Document) []pipelineGraphNodeJSON {
	ids := make([]string, 0, len(d.Nodes))
	for id := range d.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]pipelineGraphNodeJSON, 0, len(ids))
	for _, id := range ids {
		n := d.Nodes[id]
		out = append(out, pipelineGraphNodeJSON{ID: id, Kind: n.Type, Prompt: n.Prompt, Question: n.Question})
	}
	return out
}

func (s *Server) pipelineDetail(w http.ResponseWriter, r *http.Request) {
	sw, ok := s.pipelineFind(w, r, app.ActionViewSwarm)
	if !ok {
		return
	}
	view, err := s.pipelineRunView(r, sw)
	if err != nil {
		apiError(w, r, err)
		return
	}
	writeJSON(w, 200, view)
}

// pipelineAnswer — старый вход ответа шлюза, пока панель не перешла на
// API запросов роя (#216): тонкая обёртка над answerSwarmRequest, которая
// находит узловой запрос по шагу и отдаёт вид прогона.
func (s *Server) pipelineAnswer(w http.ResponseWriter, r *http.Request) {
	sw, ok := s.pipelineFind(w, r, app.ActionManageSwarm)
	if !ok {
		return
	}
	var input struct {
		Answer   string `json:"answer"`
		Response string `json:"response"`
		Sequence int64  `json:"visitSequence"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384))
	dec.DisallowUnknownFields()
	if dec.Decode(&input) != nil || input.Sequence < 1 || input.Sequence > 9007199254740991 || input.Answer == "" {
		writeDetail(w, 400, "invalid pipeline answer")
		return
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		writeDetail(w, 400, "invalid pipeline answer")
		return
	}
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		writeDetail(w, 401, "authentication required")
		return
	}
	req, found, err := s.nodeRequestAt(r.Context(), sw.ID, input.Sequence, r.PathValue("node"))
	if err != nil {
		apiError(w, r, err)
		return
	}
	if !found {
		writeDetail(w, 409, "pipeline visit is no longer awaiting an answer")
		return
	}
	// Повтор потерянного ответа решает хранилище в той же транзакции: он
	// принимается только от автора с действующей сессией и правами, с тем же
	// исходом и текстом; иначе 409.
	_, _, err = s.answerSwarmRequest(r, user, store.SwarmAnswer{SwarmID: sw.ID, RequestID: req.ID, UserID: user.ID, Outcome: input.Answer, Response: input.Response})
	if err != nil {
		swarmRequestError(w, r, err)
		return
	}
	current, err := s.store.GetSwarm(r.Context(), sw.ID)
	if err != nil {
		apiError(w, r, err)
		return
	}
	view, err := s.pipelineRunView(r, current)
	if err != nil {
		apiError(w, r, err)
		return
	}
	writeJSON(w, 200, view)
}

// nodeRequestAt находит узловой запрос посещения: узел из пути должен
// совпасть с узлом шага, иначе запроса для этого адреса нет.
func (s *Server) nodeRequestAt(ctx context.Context, swarmID string, sequence int64, node string) (store.SwarmRequest, bool, error) {
	steps, err := s.store.SwarmSteps(ctx, swarmID)
	if err != nil {
		return store.SwarmRequest{}, false, err
	}
	requests, err := s.store.SwarmRequests(ctx, swarmID)
	if err != nil {
		return store.SwarmRequest{}, false, err
	}
	for _, v := range steps {
		if v.Sequence != sequence || v.NodeID != node {
			continue
		}
		for _, req := range requests {
			if req.Origin == "node" && req.Sequence != nil && *req.Sequence == sequence {
				return req, true, nil
			}
		}
	}
	return store.SwarmRequest{}, false, nil
}
