package server

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"brood.dev/go/brood/store"
)

type childGroupSummaryJSON struct {
	ParentSequence    int64  `json:"parentSequence"`
	Pass              int64  `json:"pass"`
	ParentUID         string `json:"parentUid"`
	ParentIncarnation int64  `json:"parentIncarnation"`
	Mode              string `json:"mode"`
	Total             int    `json:"total"`
	Quorum            int    `json:"quorum"`
	State             string `json:"state"`
	Revision          int64  `json:"revision"`
	Interrupted       bool   `json:"interrupted"`
	Registered        int    `json:"registered"`
	Open              int    `json:"open"`
	Started           int    `json:"started"`
}

type childHistoryTaskJSON struct {
	ID                  string `json:"id"`
	RequestID           string `json:"requestId"`
	Role                string `json:"role"`
	ExecutorUID         string `json:"executorUid"`
	ExecutorIncarnation int64  `json:"executorIncarnation"`
	State               string `json:"state"`
	Delivery            string `json:"delivery"`
	DeliveryDetail      string `json:"deliveryDetail,omitempty"`
	Task                string `json:"task"`
	Outcome             string `json:"outcome,omitempty"`
	Report              string `json:"report,omitempty"`
	Revision            int64  `json:"revision"`
	CreatedAt           string `json:"createdAt"`
	FinishedAt          string `json:"finishedAt,omitempty"`
}

type childHistoryGroupJSON struct {
	childGroupSummaryJSON
	Readiness string                 `json:"readiness"`
	AbortedBy string                 `json:"abortedBy,omitempty"`
	AbortedAt string                 `json:"abortedAt,omitempty"`
	Children  []childHistoryTaskJSON `json:"children"`
}

func childGroupSummaryView(summary store.SwarmChildGroupSummary) childGroupSummaryJSON {
	return childGroupSummaryJSON{
		ParentSequence: summary.Parent.Sequence, Pass: summary.Parent.Pass,
		ParentUID: summary.Parent.Principal.ActorUID, ParentIncarnation: summary.Parent.Principal.Incarnation,
		Mode: summary.Policy.Mode, Total: summary.Policy.Total, Quorum: summary.Policy.Quorum,
		State: summary.State, Revision: summary.Revision, Interrupted: summary.Interrupted,
		Registered: summary.Registered, Open: summary.Open, Started: summary.Started,
	}
}

func childHistoryView(group store.SwarmChildJoin) childHistoryGroupJSON {
	summary := store.SwarmChildGroupSummary{Parent: group.Parent, Policy: group.Policy, State: group.State, Revision: group.Revision, Interrupted: group.Interrupted, Registered: len(group.Children), Open: group.Evaluation.Open}
	result := childHistoryGroupJSON{Readiness: group.Evaluation.State, AbortedBy: group.AbortedBy, AbortedAt: group.AbortedAt, Children: []childHistoryTaskJSON{}}
	for _, task := range group.Children {
		if task.State == "running" || task.State == "completed" {
			summary.Started++
		}
		result.Children = append(result.Children, childHistoryTaskJSON{
			ID: task.ID, RequestID: task.RequestID, Role: task.Role,
			ExecutorUID: task.Executor.ActorUID, ExecutorIncarnation: task.Executor.Incarnation,
			State: task.State, Delivery: task.Delivery, DeliveryDetail: task.DeliveryDetail,
			Task: task.Payload, Outcome: task.Outcome, Report: task.Report,
			Revision: task.Revision, CreatedAt: task.CreatedAt, FinishedAt: task.FinishedAt,
		})
	}
	result.childGroupSummaryJSON = childGroupSummaryView(summary)
	return result
}

func childHistoryPagination(raw string) (before int64, limit int, valid bool) {
	query, err := url.ParseQuery(raw)
	if err != nil {
		return 0, 0, false
	}
	for key, values := range query {
		if (key != "before" && key != "limit") || len(values) != 1 {
			return 0, 0, false
		}
	}
	limit = 25
	if values, ok := query["limit"]; ok {
		limit, err = strconv.Atoi(values[0])
		if err != nil || limit < 1 || limit > 100 {
			return 0, 0, false
		}
	}
	if values, ok := query["before"]; ok {
		before, err = strconv.ParseInt(values[0], 10, 64)
		if err != nil || before < 0 {
			return 0, 0, false
		}
	}
	return before, limit, true
}

func (s *Server) handleChildHistoryList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if _, ok := s.visibleSwarmUser(w, r); !ok {
		return
	}
	before, limit, ok := childHistoryPagination(r.URL.RawQuery)
	if !ok {
		writeDetail(w, http.StatusBadRequest, "child history requires limit 1..100 and a nonnegative before cursor; unknown or duplicate parameters are not accepted")
		return
	}
	page, err := s.store.SwarmChildGroups(r.Context(), r.PathValue("id"), before, limit)
	if err != nil {
		writeDetail(w, http.StatusInternalServerError, "child history unavailable")
		return
	}
	if _, ok := s.visibleSwarmUser(w, r); !ok {
		return
	}
	items := make([]childGroupSummaryJSON, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, childGroupSummaryView(item))
	}
	writeJSON(w, http.StatusOK, struct {
		Items      []childGroupSummaryJSON `json:"items"`
		NextBefore int64                   `json:"nextBefore"`
	}{items, page.NextBefore})
}

func (s *Server) handleChildHistoryGet(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if _, ok := s.visibleSwarmUser(w, r); !ok {
		return
	}
	sequence, err := strconv.ParseInt(r.PathValue("sequence"), 10, 64)
	if err != nil || sequence < 1 || r.URL.RawQuery != "" {
		writeDetail(w, http.StatusBadRequest, "child history requires a positive parent sequence and accepts no query parameters")
		return
	}
	group, err := s.store.GetSwarmChildHistory(r.Context(), r.PathValue("id"), sequence)
	if errors.Is(err, store.ErrNotFound) {
		writeDetail(w, http.StatusNotFound, "child group not found")
		return
	}
	if err != nil {
		writeDetail(w, http.StatusInternalServerError, "child history unavailable")
		return
	}
	if _, ok := s.visibleSwarmUser(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, childHistoryView(group))
}
