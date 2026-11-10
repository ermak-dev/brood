package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/store"
)

// maxSwarmStartBody bounds the start request: a definition name and a goal
// of up to 4096 bytes each and a commit. JSON may escape every byte of them
// sixfold (Go writes "<" as \u003c), so a goal within store.MaxSwarmGoalBytes
// must not be refused as an oversized body.
const maxSwarmStartBody = 64 << 10

type swarmDefinitionJSON struct {
	Name     string `json:"name"`
	Version  int64  `json:"version"`
	Revision string `json:"revision"`
}

type swarmSourceJSON struct {
	Origin string `json:"origin"`
	Commit string `json:"commit"`
}

type swarmStepJSON struct {
	Sequence int64  `json:"sequence"`
	NodeID   string `json:"nodeId"`
	State    string `json:"state"`
	// Detail — причина refused или unconfirmed шага актора (#212).
	Detail string `json:"detail,omitempty"`
}

// swarmExecutionJSON — исполнение роя после запуска или остановки: состояние,
// личность снимка, источник, задача текущего прохода (#242) и последний шаг.
type swarmExecutionJSON struct {
	SwarmID    string               `json:"swarmId"`
	Execution  string               `json:"execution"`
	Definition *swarmDefinitionJSON `json:"definition"`
	Source     *swarmSourceJSON     `json:"source"`
	Goal       string               `json:"goal,omitempty"`
	Step       *swarmStepJSON       `json:"step"`
}

// mountSwarmExecutionAPI — запуск и остановка роя (#211). Запросы роя и
// доставка ответа — отдельные маршруты (#210); инструкцию шага доставляет
// диспетчер после коммита запуска (#212), отдельного маршрута у неё нет.
func (s *Server) mountSwarmExecutionAPI() {
	s.mux.HandleFunc("POST /api/swarms/{id}/start", s.handleSwarmStart)
	s.mux.HandleFunc("POST /api/swarms/{id}/stop", s.handleSwarmStop)
}

// visibleSwarmUser пропускает дальше только видящего рой: постороннему рой
// скрыт как 404. Право управлять проверяет хранилище в транзакции записи.
func (s *Server) visibleSwarmUser(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	user, ok := currentUser(w, r)
	if !ok {
		return store.User{}, false
	}
	visible, err := s.app.Can(user.ID, app.ActionViewSwarm, app.SwarmRes(r.PathValue("id")))
	if err != nil {
		apiError(w, r, err)
		return store.User{}, false
	}
	if !visible {
		writeDetail(w, http.StatusNotFound, "swarm not found")
		return store.User{}, false
	}
	return user, true
}

// handleSwarmStart: POST /api/swarms/{id}/start {definition, goal, commit?}.
// Без commit первый запуск пиннит HEAD repository проекта, разрешённый
// сервером. Задача прохода проверяется до этого: недопустимая задача не
// стоит запроса к upstream с credential проекта (#242).
func (s *Server) handleSwarmStart(w http.ResponseWriter, r *http.Request) {
	user, ok := s.visibleSwarmUser(w, r)
	if !ok {
		return
	}
	in, ok := readSwarmStart(w, r)
	if !ok {
		return
	}
	if err := startGoalError(in.Goal); err != nil {
		swarmExecutionError(w, r, err)
		return
	}
	in.SwarmID, in.UserID = r.PathValue("id"), user.ID
	if in.Commit == "" {
		// Сеть — до транзакции записи и вне её (#228).
		origin, commit, err := s.startCommit(r.Context(), in.SwarmID, user.ID)
		if err != nil {
			swarmExecutionError(w, r, err)
			return
		}
		in.Origin, in.Commit = origin, commit
	}
	sw, err := s.store.StartSwarm(r.Context(), in, func(ctx context.Context) bool {
		return secretRequestSession(ctx, s, r, user.ID)
	})
	if err != nil {
		swarmExecutionError(w, r, err)
		return
	}
	s.writeSwarmExecution(w, r, sw)
	// Первый шаг actor-узла ждёт доставки: диспетчер (#212) берёт его после
	// коммита запуска, ответ запуска его не ждёт и показывает шаг pending.
	s.steps.kick(sw.ID)
}

// handleSwarmStop: POST /api/swarms/{id}/stop.
func (s *Server) handleSwarmStop(w http.ResponseWriter, r *http.Request) {
	user, ok := s.visibleSwarmUser(w, r)
	if !ok {
		return
	}
	sw, err := s.store.StopSwarm(r.Context(), r.PathValue("id"), user.ID, func(ctx context.Context) bool {
		return secretRequestSession(ctx, s, r, user.ID)
	})
	if err != nil {
		swarmExecutionError(w, r, err)
		return
	}
	s.writeSwarmExecution(w, r, sw)
}

// readSwarmStart разбирает тело строго: объект с уникальными ключами,
// обязательные строки definition и goal и необязательная строка commit.
// Текст задачи проверяет store.ValidSwarmGoal с причиной, а не этот разбор.
func readSwarmStart(w http.ResponseWriter, r *http.Request) (store.SwarmStart, bool) {
	const invalid = "invalid swarm start: expected {definition, goal, commit?} with a definition name and a goal"
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSwarmStartBody))
	if err != nil {
		writeDetail(w, http.StatusBadRequest, invalid)
		return store.SwarmStart{}, false
	}
	fields, err := proxmoxObject(string(data))
	if err != nil {
		writeDetail(w, http.StatusBadRequest, invalid)
		return store.SwarmStart{}, false
	}
	var in store.SwarmStart
	goal := false
	for key, raw := range fields {
		var target *string
		switch key {
		case "definition":
			target = &in.Definition
		case "goal":
			target, goal = &in.Goal, true
		case "commit":
			target = &in.Commit
		default:
			writeDetail(w, http.StatusBadRequest, invalid)
			return store.SwarmStart{}, false
		}
		if !syncString(raw, target) {
			writeDetail(w, http.StatusBadRequest, invalid)
			return store.SwarmStart{}, false
		}
	}
	if in.Definition == "" || !goal {
		writeDetail(w, http.StatusBadRequest, invalid)
		return store.SwarmStart{}, false
	}
	return in, true
}

// startGoalError — отказ задачи прохода до любой записи и сети: правила
// хранилища (store.ValidSwarmGoal) и закрывающий тег конверта сообщения,
// который гость не примет ни в одной инструкции прохода (stepInstruction).
func startGoalError(goal string) error {
	if err := store.ValidSwarmGoal(goal); err != nil {
		return err
	}
	if strings.Contains(goal, envelopeClosingTag) {
		return fmt.Errorf("%w: the goal contains the message envelope closing tag", store.ErrSwarmGoalInvalid)
	}
	return nil
}

// swarmExecutionError переводит отказ запуска или остановки в статус;
// причина отказа уходит вызывающему строкой.
func swarmExecutionError(w http.ResponseWriter, r *http.Request, err error) {
	var fit *store.RoleFitError
	var head *gitHeadError
	switch {
	case errors.As(err, &head):
		writeDetail(w, head.status, head.Error())
	case errors.Is(err, store.ErrSwarmRequestDenied):
		writeDetail(w, http.StatusForbidden, "swarm management denied")
	case errors.Is(err, store.ErrWorkflowDefinitionMissing):
		writeDetail(w, http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrNotFound):
		writeDetail(w, http.StatusNotFound, "swarm not found")
	case errors.Is(err, store.ErrSwarmExecutionState), errors.Is(err, store.ErrSwarmGraphPinned), errors.Is(err, store.ErrSwarmSourceChanged), errors.As(err, &fit):
		writeDetail(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrWorkflowGraphInvalid), errors.Is(err, store.ErrSwarmSourceInvalid), errors.Is(err, store.ErrSwarmGoalInvalid):
		writeDetail(w, http.StatusUnprocessableEntity, err.Error())
	default:
		apiError(w, r, err)
	}
}

// writeSwarmExecution отвечает исполнением роя с задачей текущего прохода и
// его последним шагом.
func (s *Server) writeSwarmExecution(w http.ResponseWriter, r *http.Request, sw store.Swarm) {
	steps, err := s.store.SwarmSteps(r.Context(), sw.ID)
	if err != nil {
		apiError(w, r, err)
		return
	}
	out := swarmExecutionJSON{SwarmID: sw.ID, Execution: sw.Execution}
	if sw.Graph != nil {
		out.Definition = &swarmDefinitionJSON{Name: sw.DefinitionName, Version: sw.DefinitionVersion, Revision: sw.DefinitionRevision}
	}
	if sw.Source != nil {
		out.Source = &swarmSourceJSON{Origin: sw.Source.Origin, Commit: sw.Source.Commit}
	}
	out.Goal = currentPassGoal(steps)
	if len(steps) > 0 {
		last := steps[len(steps)-1]
		out.Step = &swarmStepJSON{Sequence: last.Sequence, NodeID: last.NodeID, State: last.State, Detail: last.Detail}
	}
	writeJSON(w, http.StatusOK, out)
}

// currentPassGoal — задача прохода последнего шага роя: её хранит первый шаг
// этого прохода (#242). До первого запуска задачи нет.
func currentPassGoal(steps []store.PipelineVisit) string {
	if len(steps) == 0 {
		return ""
	}
	pass := steps[len(steps)-1].Pass
	for i := len(steps) - 1; i >= 0; i-- {
		if steps[i].Sequence == pass {
			return steps[i].Goal
		}
	}
	return ""
}
