package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"brood.dev/go/brood/store"
)

type passComparisonSelection struct {
	swarm    string
	pass     int64
	download bool
}

func parsePassComparisonSelection(raw string) (passComparisonSelection, bool) {
	query, err := url.ParseQuery(raw)
	if err != nil {
		return passComparisonSelection{}, false
	}
	for key, values := range query {
		if len(values) != 1 || key != "swarm" && key != "pass" && key != "download" {
			return passComparisonSelection{}, false
		}
	}
	pass, err := strconv.ParseInt(query.Get("pass"), 10, 64)
	if err != nil || pass < 1 || strconv.FormatInt(pass, 10) != query.Get("pass") || query.Get("swarm") == "" || len(query.Get("swarm")) > 256 {
		return passComparisonSelection{}, false
	}
	if values, set := query["download"]; set && values[0] != "0" && values[0] != "1" {
		return passComparisonSelection{}, false
	}
	return passComparisonSelection{swarm: query.Get("swarm"), pass: pass, download: query.Get("download") == "1"}, true
}

func (s *Server) handleSwarmPassComparison(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if _, ok := s.visibleSwarmUser(w, r); !ok {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeDetail(w, http.StatusMethodNotAllowed, "pass comparison is read-only")
		return
	}
	referencePass, err := strconv.ParseInt(r.PathValue("pass"), 10, 64)
	selection, valid := parsePassComparisonSelection(r.URL.RawQuery)
	if err != nil || referencePass < 1 || strconv.FormatInt(referencePass, 10) != r.PathValue("pass") || !valid {
		writeDetail(w, http.StatusBadRequest, "pass comparison requires canonical positive pass numbers, one swarm and pass selection, and optional download=0 or download=1")
		return
	}
	if _, ok := s.visibleSwarmIDUser(w, r, selection.swarm); !ok {
		return
	}
	var reference, candidate store.SwarmPassTrace
	err = s.store.WithTx(r.Context(), func(ctx context.Context) error {
		var err error
		reference, err = s.store.GetSwarmPassTrace(ctx, r.PathValue("id"), referencePass)
		if err != nil {
			return err
		}
		candidate, err = s.store.GetSwarmPassTrace(ctx, selection.swarm, selection.pass)
		return err
	})
	if err != nil {
		passComparisonError(w, err)
		return
	}
	comparison, err := store.CompareSwarmPassTraces(reference, candidate)
	if err != nil {
		passComparisonError(w, err)
		return
	}
	data, err := json.Marshal(comparison)
	if err != nil {
		passComparisonError(w, err)
		return
	}
	if len(data) > store.MaxPassTraceBytes {
		passComparisonError(w, store.ErrPassTraceLimit)
		return
	}
	if _, ok := s.visibleSwarmUser(w, r); !ok {
		return
	}
	if _, ok := s.visibleSwarmIDUser(w, r, selection.swarm); !ok {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if selection.download {
		w.Header().Set("Content-Disposition", `attachment; filename="brood-pass-comparison.json"`)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(data, '\n'))
}

func passComparisonError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeDetail(w, http.StatusNotFound, "pass not found")
	case errors.Is(err, store.ErrPassTraceLimit):
		writeDetail(w, http.StatusRequestEntityTooLarge, "pass comparison exceeds the trace budget; no partial comparison was returned")
	default:
		writeDetail(w, http.StatusInternalServerError, "pass comparison unavailable")
	}
}
