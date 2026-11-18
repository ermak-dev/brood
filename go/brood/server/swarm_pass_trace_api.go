package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"brood.dev/go/brood/store"
)

func passTraceDownload(raw string) (bool, bool) {
	query, err := url.ParseQuery(raw)
	if err != nil {
		return false, false
	}
	for key, values := range query {
		if key != "download" || len(values) != 1 || values[0] != "0" && values[0] != "1" {
			return false, false
		}
	}
	return query.Get("download") == "1", true
}

func (s *Server) handleSwarmPassTrace(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if _, ok := s.visibleSwarmUser(w, r); !ok {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeDetail(w, http.StatusMethodNotAllowed, "pass trace is read-only")
		return
	}
	pass, err := strconv.ParseInt(r.PathValue("pass"), 10, 64)
	download, valid := passTraceDownload(r.URL.RawQuery)
	if err != nil || pass < 1 || strconv.FormatInt(pass, 10) != r.PathValue("pass") || !valid {
		writeDetail(w, http.StatusBadRequest, "pass trace requires a canonical positive pass number and accepts only download=0 or download=1")
		return
	}
	trace, err := s.store.GetSwarmPassTrace(r.Context(), r.PathValue("id"), pass)
	if errors.Is(err, store.ErrNotFound) {
		writeDetail(w, http.StatusNotFound, "pass not found")
		return
	}
	if errors.Is(err, store.ErrPassTraceLimit) {
		writeDetail(w, http.StatusRequestEntityTooLarge, store.ErrPassTraceLimit.Error())
		return
	}
	if err != nil {
		writeDetail(w, http.StatusInternalServerError, "pass trace unavailable")
		return
	}
	data, err := json.Marshal(passTraceView(trace))
	if err != nil {
		writeDetail(w, http.StatusInternalServerError, "pass trace unavailable")
		return
	}
	if len(data) > store.MaxPassTraceBytes {
		writeDetail(w, http.StatusRequestEntityTooLarge, store.ErrPassTraceLimit.Error())
		return
	}
	if _, ok := s.visibleSwarmUser(w, r); !ok {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if download {
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="brood-pass-%d.json"`, pass))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(data, '\n'))
}
