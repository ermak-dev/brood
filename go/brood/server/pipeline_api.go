package server

import (
	"brood.dev/go/brood/piprender"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/auth"
	"brood.dev/go/brood/pipeline"
	"brood.dev/go/brood/store"
)

type pipelineNodeJSON struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Status  string `json:"status"`
	Label   string `json:"label"`
	Outcome string `json:"outcome,omitempty"`
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
	StartedAt   *string            `json:"startedAt"`
	CurrentNode string             `json:"currentNode,omitempty"`
	Nodes       []pipelineNodeJSON `json:"nodes"`
	Gate        *pipelineGateJSON  `json:"gate,omitempty"`
}

func (s *Server) mountPipelineAPI() {
	s.mux.HandleFunc("GET /api/swarms/{id}/pipeline-runs", s.pipelineList)
	s.mux.HandleFunc("GET /api/pipeline-runs/{run}", s.pipelineDetail)
	s.mux.HandleFunc("GET /api/pipeline-runs/{run}/graph", s.pipelineGraph)
	s.mux.HandleFunc("POST /api/pipeline-runs/{run}/nodes/{node}/answer", s.pipelineAnswer)
}

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
	runs, err := s.store.ListPipelineRuns(r.Context(), id)
	if err != nil {
		apiError(w, r, err)
		return
	}
	out := make([]pipelineRunJSON, 0, len(runs))
	for _, run := range runs {
		v, err := s.pipelineView(r, run)
		if err != nil {
			apiError(w, r, err)
			return
		}
		out = append(out, v)
	}
	writeJSON(w, 200, out)
}

// pipelineFind scans swarm IDs because the store deliberately exposes only scoped run reads.
func (s *Server) pipelineFind(w http.ResponseWriter, r *http.Request, action app.Action) (store.PipelineRun, bool) {
	user, ok := currentUser(w, r)
	if !ok {
		return store.PipelineRun{}, false
	}
	swarms, err := s.gateway.Swarms()
	if err != nil {
		apiError(w, r, err)
		return store.PipelineRun{}, false
	}
	for _, sw := range swarms {
		allowed, e := s.app.Can(user.ID, action, app.SwarmRes(sw.ID))
		if e != nil {
			apiError(w, r, e)
			return store.PipelineRun{}, false
		}
		if !allowed {
			continue
		}
		run, e := s.store.GetPipelineRun(r.Context(), sw.ID, r.PathValue("run"))
		if errors.Is(e, store.ErrNotFound) {
			continue
		}
		if e != nil {
			apiError(w, r, e)
			return store.PipelineRun{}, false
		}
		return run, true
	}
	writeDetail(w, 404, "pipeline run not found")
	return store.PipelineRun{}, false
}

func (s *Server) pipelineView(r *http.Request, run store.PipelineRun) (pipelineRunJSON, error) {
	d, err := pipeline.Parse(string(run.Graph), "pipeline")
	if err != nil {
		return pipelineRunJSON{}, err
	}
	visits, err := s.store.ListPipelineVisits(r.Context(), run.SwarmID, run.ID)
	if err != nil {
		return pipelineRunJSON{}, err
	}
	result := pipelineRunJSON{ID: run.ID, Name: d.Name, Status: run.Status, Nodes: []pipelineNodeJSON{}}
	for _, v := range visits {
		n := d.Nodes[v.NodeID]
		row := pipelineNodeJSON{ID: v.NodeID, Kind: n.Type, Label: v.NodeID, Status: v.State}
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
	if len(visits) > 0 {
		v := visits[len(visits)-1]
		result.CurrentNode = v.NodeID
		n := d.Nodes[v.NodeID]
		if v.State == "waiting" && n.Type == "human" && (run.Status == "running" || run.Status == "waiting_human") {
			user, ok := auth.UserFromContext(r.Context())
			if !ok {
				return pipelineRunJSON{}, errors.New("authentication required")
			}
			allowed, err := s.app.Can(user.ID, app.ActionManageSwarm, app.SwarmRes(run.SwarmID))
			if err != nil {
				return pipelineRunJSON{}, err
			}
			if !allowed {
				return result, nil
			}
			gate := &pipelineGateJSON{RunID: run.ID, NodeID: v.NodeID, Sequence: v.Sequence, Question: n.Question, Kind: "multiple_choice", Options: []pipelineOptionJSON{}}
			for _, o := range n.Options {
				gate.Options = append(gate.Options, pipelineOptionJSON{ID: o.Outcome, Label: o.Label})
			}
			result.Gate = gate
		}
	}
	return result, nil
}
func (s *Server) pipelineGraph(w http.ResponseWriter, r *http.Request) {
	run, ok := s.pipelineFind(w, r, app.ActionViewSwarm)
	if !ok {
		return
	}
	d, err := pipeline.Parse(string(run.Graph), "pipeline")
	if err != nil {
		apiError(w, r, err)
		return
	}
	view, err := s.pipelineView(r, run)
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
	const styles = `<style>.state-active rect{fill:#fef3c7;stroke:#b45309}.state-done rect{fill:#dcfce7;stroke:#15803d}.state-failed rect{fill:#fee2e2;stroke:#b91c1c}.state-skipped rect{fill:#e5e7eb;stroke:#6b7280}</style>`
	svg = strings.Replace(svg, `role="img">`, `role="img">`+styles, 1)
	writeJSON(w, 200, struct {
		SVG string `json:"svg"`
	}{svg})
}

func (s *Server) pipelineDetail(w http.ResponseWriter, r *http.Request) {
	run, ok := s.pipelineFind(w, r, app.ActionViewSwarm)
	if !ok {
		return
	}
	view, err := s.pipelineView(r, run)
	if err != nil {
		apiError(w, r, err)
		return
	}
	writeJSON(w, 200, view)
}
func pipelineAnswerError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeDetail(w, 404, "pipeline run not found")
	} else if errors.Is(err, store.ErrPipelineVisitConflict) {
		writeDetail(w, 409, "pipeline visit is no longer awaiting an answer")
	} else {
		apiError(w, r, err)
	}
}

func (s *Server) pipelineAnswer(w http.ResponseWriter, r *http.Request) {
	run, ok := s.pipelineFind(w, r, app.ActionManageSwarm)
	if !ok {
		return
	}
	var input struct {
		Answer   string `json:"answer"`
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
	visits, err := s.store.ListPipelineVisits(r.Context(), run.SwarmID, run.ID)
	if err != nil {
		apiError(w, r, err)
		return
	}
	eligible := false
	for i, visit := range visits {
		if visit.Sequence == input.Sequence && visit.NodeID == r.PathValue("node") {
			eligible = (i == len(visits)-1 && visit.State == "waiting") ||
				(visit.State == "answered" && visit.Answer != nil && *visit.Answer == input.Answer)
			break
		}
	}
	if !eligible {
		writeDetail(w, 409, "pipeline visit is no longer awaiting an answer")
		return
	}
	d, err := pipeline.Parse(string(run.Graph), "pipeline")
	if err != nil {
		apiError(w, r, err)
		return
	}
	valid := false
	for _, o := range d.Nodes[r.PathValue("node")].Options {
		if o.Outcome == input.Answer {
			valid = true
		}
	}
	if !valid {
		writeDetail(w, 400, "answer is not a human option")
		return
	}
	if err = s.store.AnswerPipelineVisit(r.Context(), run.SwarmID, run.ID, input.Sequence, input.Answer); err != nil {
		pipelineAnswerError(w, r, err)
		return
	}
	run, err = s.store.GetPipelineRun(r.Context(), run.SwarmID, run.ID)
	if err != nil {
		apiError(w, r, err)
		return
	}
	view, err := s.pipelineView(r, run)
	if err != nil {
		apiError(w, r, err)
		return
	}
	writeJSON(w, 200, view)
}
