package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/auth"
	"brood.dev/go/brood/store"
)

// accessSwarm находит живой рой запроса для его владельца или
// администратора; недоступный рой скрыт как 404.
func (s *Server) accessSwarm(w http.ResponseWriter, r *http.Request) (store.User, app.Swarm, bool) {
	user, ok := currentUser(w, r)
	if !ok {
		return user, app.Swarm{}, false
	}
	swarm, err := s.app.GetSwarm(r.Context(), user.ID, r.PathValue("id"))
	if errors.Is(err, app.ErrNotAllowed) {
		err = fmt.Errorf("%w: swarm not found", app.ErrNotFound)
	}
	if err != nil {
		apiError(w, r, err)
		return user, app.Swarm{}, false
	}
	return user, swarm, true
}

// decodeAccessBody читает строгий JSON-объект тела: без лишних полей и хвоста.
func decodeAccessBody(w http.ResponseWriter, r *http.Request, body any) bool {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	trimmed := bytes.TrimSpace(data)
	if err != nil || len(trimmed) == 0 || trimmed[0] != '{' {
		writeDetail(w, http.StatusBadRequest, "access request must be a JSON object")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(body); err != nil {
		writeDetail(w, http.StatusBadRequest, "invalid access request fields")
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeDetail(w, http.StatusBadRequest, "access request must contain a single JSON object")
		return false
	}
	return true
}

// requestLogger — журнал запроса или общий журнал процесса.
func requestLogger(ctx context.Context) *slog.Logger {
	if lg := auth.LoggerFromContext(ctx); lg != nil {
		return lg
	}
	return slog.Default()
}

// sleepContext ждёт d или отмену ctx; false — ctx отменён.
func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
