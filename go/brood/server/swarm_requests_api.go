package server

// Запросы роя оператору (#210): список, ответ и повтор доставки ответа
// спросившему актору. Ответ пишется одной транзакцией хранилища вместе с
// продвижением графа у узлового запроса; доставка ответа актору идёт после
// коммита каналом оператора (тем же, что send), и её сбой ответа не
// откатывает. Клиент видит только фиксированные публичные сообщения.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/auth"
	"brood.dev/go/brood/command"
	"brood.dev/go/brood/store"
)

// requestDeliveryTimeout ограничивает доставку ответа и инструкции шага: она
// переживает разрыв соединения клиента, но не держит обработчик бесконечно.
// Гость ждёт квитанцию доставки до command.ReceiptWait, и его relay держит
// канал command.CallTimeout(0); сверх того — открытие канала relay (до 10
// секунд) и проверки до и после отправки. Короче — ожидание квитанции
// обрывалось бы таймаутом канала, а не приходило вердиктом (#260).
var requestDeliveryTimeout = command.CallTimeout(0) + 10*time.Second

// maxRequestAnswerBytes вмещает ответ предельного размера после
// экранирования JSON.
const maxRequestAnswerBytes = 8 * store.MaxSwarmRequestTextBytes

// questionExcerptBytes — длина фрагмента вопроса в сообщении актору.
const questionExcerptBytes = 200

type swarmRequestOptionJSON struct {
	Label   string `json:"label"`
	Outcome string `json:"outcome"`
}

type swarmRequestActorJSON struct {
	Name        string `json:"name,omitempty"`
	Incarnation int64  `json:"incarnation"`
}

type swarmRequestJSON struct {
	ID          string                   `json:"id"`
	SwarmID     string                   `json:"swarmId"`
	Origin      string                   `json:"origin"`
	Sequence    *int64                   `json:"sequence,omitempty"`
	NodeID      string                   `json:"nodeId,omitempty"`
	Actor       *swarmRequestActorJSON   `json:"actor,omitempty"`
	Kind        string                   `json:"kind"`
	Question    string                   `json:"question"`
	Context     string                   `json:"context,omitempty"`
	Options     []swarmRequestOptionJSON `json:"options"`
	State       string                   `json:"state"`
	CreatedAt   string                   `json:"createdAt"`
	AnsweredAt  *string                  `json:"answeredAt,omitempty"`
	AnsweredBy  string                   `json:"answeredBy,omitempty"`
	Outcome     *string                  `json:"outcome,omitempty"`
	OptionLabel string                   `json:"optionLabel,omitempty"`
	Response    *string                  `json:"response,omitempty"`
	// Delivery — доставка ответа актору: pending, delivered, failed (ответ
	// заведомо не ушёл, повтор безопасен) или unconfirmed (ушёл без
	// квитанции, повтор может дать дубль, #260).
	Delivery string `json:"delivery,omitempty"`
	// DeliveryError — публичная причина failed или unconfirmed этого
	// вызова; у unconfirmed она предупреждает о возможном дубле.
	DeliveryError string `json:"deliveryError,omitempty"`
}

func (s *Server) mountSwarmRequests() {
	s.mux.HandleFunc("GET /api/swarms/{id}/requests", s.swarmRequestList)
	s.mux.HandleFunc("POST /api/swarms/{id}/requests/{request}/answer", s.swarmRequestAnswer)
	s.mux.HandleFunc("POST /api/swarms/{id}/requests/{request}/deliver", s.swarmRequestDeliver)
}

// swarmRequestError отвечает фиксированным набором публичных сообщений:
// текст ошибок хранилища и адаптеров клиенту не уходит (тот же приём, что у
// синхронизации проекта). Внутренний сбой логируется с причиной.
func swarmRequestError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, app.ErrNotFound):
		writeDetail(w, http.StatusNotFound, "swarm request not found")
	case errors.Is(err, store.ErrSwarmRequestDenied), errors.Is(err, app.ErrNotAllowed):
		writeDetail(w, http.StatusForbidden, "answer authorization changed or access denied")
	case errors.Is(err, store.ErrSwarmRequestStale):
		writeDetail(w, http.StatusConflict, "request actor is stale")
	case errors.Is(err, store.ErrPipelineVisitConflict):
		writeDetail(w, http.StatusConflict, "swarm request is no longer open")
	case errors.Is(err, store.ErrSwarmRequestInvalid):
		writeDetail(w, http.StatusBadRequest, "answer does not fit the request: check outcome and response")
	default:
		requestLogger(r.Context()).Error("swarm request failed", "err", err.Error())
		writeDetail(w, http.StatusInternalServerError, "internal error")
	}
}

func requestLogger(ctx context.Context) *slog.Logger {
	if lg := auth.LoggerFromContext(ctx); lg != nil {
		return lg
	}
	return slog.Default()
}

// swarmRequestAccess пропускает пользователя с правом action на рой из
// пути; иначе 404, как у остальных операций роя: закрытый рой не виден.
func (s *Server) swarmRequestAccess(w http.ResponseWriter, r *http.Request, action app.Action) (store.User, bool) {
	user, ok := currentUser(w, r)
	if !ok {
		return store.User{}, false
	}
	allowed, err := s.app.Can(user.ID, action, app.SwarmRes(r.PathValue("id")))
	if err != nil && !errors.Is(err, app.ErrNotFound) {
		swarmRequestError(w, r, err)
		return store.User{}, false
	}
	if !allowed {
		writeDetail(w, http.StatusNotFound, "swarm not found")
		return store.User{}, false
	}
	return user, true
}

// swarmRequestList отдаёт открытые запросы роя, с ?all=1 — все.
func (s *Server) swarmRequestList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.swarmRequestAccess(w, r, app.ActionViewSwarm); !ok {
		return
	}
	var all bool
	switch r.URL.Query().Get("all") {
	case "", "0", "false":
	case "1", "true":
		all = true
	default:
		writeDetail(w, http.StatusBadRequest, "invalid request filter: all must be 1 or 0")
		return
	}
	swarmID := r.PathValue("id")
	requests, err := s.store.SwarmRequests(r.Context(), swarmID)
	if err != nil {
		swarmRequestError(w, r, err)
		return
	}
	selected := []store.SwarmRequest{}
	for _, req := range requests {
		if all || req.State == "open" {
			selected = append(selected, req)
		}
	}
	views, err := s.swarmRequestViews(r.Context(), swarmID, selected)
	if err != nil {
		swarmRequestError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Results []swarmRequestJSON `json:"results"`
	}{views})
}

// swarmRequestAnswer принимает {outcome?, response?}: исход выбирает ребро
// узлового запроса, текст доставляется спросившему актору.
func (s *Server) swarmRequestAnswer(w http.ResponseWriter, r *http.Request) {
	user, ok := s.swarmRequestAccess(w, r, app.ActionManageSwarm)
	if !ok {
		return
	}
	var input struct {
		Outcome  string `json:"outcome"`
		Response string `json:"response"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestAnswerBytes))
	dec.DisallowUnknownFields()
	if dec.Decode(&input) != nil || dec.Decode(new(any)) != io.EOF {
		writeDetail(w, http.StatusBadRequest, "invalid swarm answer")
		return
	}
	answer := store.SwarmAnswer{SwarmID: r.PathValue("id"), RequestID: r.PathValue("request"), UserID: user.ID, Outcome: input.Outcome, Response: input.Response}
	req, reason, err := s.answerSwarmRequest(r, user, answer)
	if err != nil {
		swarmRequestError(w, r, err)
		return
	}
	s.writeSwarmRequest(w, r, req, reason)
}

// swarmRequestDeliver повторяет доставку записанного ответа по запросу
// оператора. Ответ, исход и граф повтор не меняет; доставленный ответ
// повторно не отправляется.
func (s *Server) swarmRequestDeliver(w http.ResponseWriter, r *http.Request) {
	user, ok := s.swarmRequestAccess(w, r, app.ActionManageSwarm)
	if !ok {
		return
	}
	if !secretRequestSession(r.Context(), s, r, user.ID) {
		swarmRequestError(w, r, store.ErrSwarmRequestDenied)
		return
	}
	req, err := s.store.GetSwarmRequest(r.Context(), r.PathValue("id"), r.PathValue("request"))
	if err != nil {
		swarmRequestError(w, r, err)
		return
	}
	if req.Delivery == "" {
		writeDetail(w, http.StatusConflict, "swarm request has no answer to deliver")
		return
	}
	req, reason := s.deliverSwarmAnswer(r.Context(), user, req, true)
	s.writeSwarmRequest(w, r, req, reason)
}

// answerSwarmRequest — единый порядок ответа для API роя и старого
// маршрута шлюза: сессия и права проверяются в транзакции записи ответа
// вместе с продвижением графа, затем после коммита ответ уходит актору.
// Повтор потерянного ответа доставляет только ещё не начатую доставку.
func (s *Server) answerSwarmRequest(r *http.Request, user store.User, answer store.SwarmAnswer) (store.SwarmRequest, string, error) {
	req, err := s.answerRequest(r.Context(), answer, func(ctx context.Context) bool {
		return secretRequestSession(ctx, s, r, user.ID)
	})
	if err != nil {
		return store.SwarmRequest{}, "", err
	}
	if req.Delivery != "pending" {
		return req, "", nil
	}
	req, reason := s.deliverSwarmAnswer(r.Context(), user, req, false)
	return req, reason, nil
}

// answerRequest записывает ответ и запускает его последствия после коммита
// — общий путь панели, CLI и Telegram (#283). authorize проверяет сессию
// вызывающего в транзакции ответа: cookie панели или сессию telegram
// привязанного чата; права пользователя answer.UserID там же проверяет
// хранилище. Доставку ответа актору вызывающий начинает сам: панель ждёт её
// исход, поллер Telegram — нет.
func (s *Server) answerRequest(ctx context.Context, answer store.SwarmAnswer, authorize func(context.Context) bool) (store.SwarmRequest, error) {
	req, err := s.store.AnswerSwarmRequest(ctx, answer, authorize)
	if err != nil {
		return store.SwarmRequest{}, err
	}
	if req.Origin == "node" {
		// Ответ мог ввести граф в actor-узел: его инструкцию доставляет
		// диспетчер шагов (#212) после коммита ответа.
		s.steps.kick(req.SwarmID)
	}
	// Кнопки ответа уходят из сообщений Telegram, где запрос ещё открыт.
	s.sender.requestClosed(req.SwarmID, req.ID)
	return req, nil
}

// deliverSwarmAnswer доставляет записанный ответ спросившему актору.
// Доставка одного ответа сериализуется в процессе: параллельный повтор
// ждёт и видит итог первого, а не шлёт актору второе сообщение. Первая
// доставка идёт только из pending, retry — ещё и из failed и unconfirmed
// (#260: ответ мог дойти, повтор может дать дубль). Сбой не откатывает
// ответ: исход попытки (requestDeliveryOutcome) записывается и
// возвращается публичной причиной. Граф доставка не трогает.
func (s *Server) deliverSwarmAnswer(parent context.Context, user store.User, req store.SwarmRequest, retry bool) (store.SwarmRequest, string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), requestDeliveryTimeout)
	defer cancel()
	unlock := s.deliveries.lock(req.ID)
	defer unlock()
	current, err := s.store.GetSwarmRequest(ctx, req.SwarmID, req.ID)
	if err != nil {
		requestLogger(ctx).Error("swarm answer delivery failed", "request", req.ID, "err", err.Error())
		return req, "delivery unavailable"
	}
	if current.Delivery != "pending" && (!retry || (current.Delivery != "failed" && current.Delivery != "unconfirmed")) {
		return current, ""
	}
	author := "operator"
	if u, err := s.store.GetUser(ctx, current.AnsweredBy); err == nil {
		author = u.Username
	}
	asker := func(ctx context.Context) (store.ManagedActor, error) { return s.requestAsker(ctx, current) }
	result, sent, err := s.deliverOperatorMessage(ctx, user.ID, current.SwarmID, asker, requestAnswerMessage(current, author), 0)
	outcome, reason := requestDeliveryOutcome(result, sent, err)
	if reason == "delivery unavailable" && err != nil {
		requestLogger(ctx).Error("swarm answer delivery failed", "request", current.ID, "err", err.Error())
	}
	recorded, err := s.store.RecordSwarmRequestDelivery(ctx, current.SwarmID, current.ID, outcome)
	if err != nil {
		requestLogger(ctx).Error("swarm answer delivery was not recorded", "request", current.ID, "outcome", outcome, "err", err.Error())
		return current, "delivery unavailable"
	}
	if outcome == "failed" && recorded.Delivery == "unconfirmed" {
		// Повтор не ушёл, но прежняя попытка могла дойти.
		reason += "; an earlier delivery is unconfirmed: " + answerMayHaveArrived
	}
	return recorded, reason
}

// requestAsker — спросивший актор той же инкарнации: ответ не уходит
// актору, пересозданному под тем же именем, и удаляемому актору.
func (s *Server) requestAsker(ctx context.Context, req store.SwarmRequest) (store.ManagedActor, error) {
	a, err := s.store.GetManagedActor(ctx, req.ActorUID)
	if err != nil {
		return store.ManagedActor{}, err
	}
	if a.SwarmID != req.SwarmID || a.Incarnation != req.ActorIncarnation || a.Phase == "deleted" || a.Phase == "deleting" || a.Desired == "deleted" {
		return store.ManagedActor{}, store.ErrSwarmRequestStale
	}
	return a, nil
}

// answerMayHaveArrived — предупреждение неизвестного исхода доставки ответа.
const answerMayHaveArrived = "the answer may have reached the actor, and a retry may deliver it twice"

// requestDeliveryOutcome переводит исход канала оператора в исход попытки
// доставки ответа и фиксированную причину (#260). delivered — только
// подтверждённый гостем приём. Сообщение, заведомо не ушедшее (sent=false:
// отказ гостя до записи в сессию, адресат не на связи, устарел, права
// сменились до отправки), — failed с причиной. Всё, что ушло в канал без
// подтверждения (квитанции нет, канал оборвался, права сменились после
// отправки), — unconfirmed: ответ мог дойти.
func requestDeliveryOutcome(result command.Result, sent bool, err error) (string, string) {
	switch {
	case err == nil && result.Outcome == "completed":
		return "delivered", ""
	case sent:
		return "unconfirmed", "delivery is unconfirmed: " + answerMayHaveArrived
	case errors.Is(err, errMessageNotSent):
		return "failed", "the answer was not sent: the actor session did not receive it"
	case errors.Is(err, errRecipientOffline):
		return "failed", "recipient is not connected"
	case errors.Is(err, app.ErrNotFound):
		return "failed", "request actor is stale"
	case errors.Is(err, app.ErrNotAllowed):
		return "failed", "delivery authorization changed"
	default:
		return "failed", "delivery unavailable"
	}
}

// requestAnswerMessage — текст ответа спросившему актору: ID запроса,
// автор и ответ. Вопрос — коротким фрагментом: вопрос и ответ по 16 КиБ
// после экранирования JSON не уложились бы в кадр сообщения relay.
func requestAnswerMessage(req store.SwarmRequest, author string) string {
	response := ""
	if req.Response != nil {
		response = *req.Response
	}
	return "[brood request " + req.ID + "] Operator " + author + " answered your question.\n" +
		"Question: " + excerpt(req.Question, questionExcerptBytes) + "\n" +
		"Answer:\n" + response + "\n\nAsk a follow-up with ask_operator."
}

// excerpt сводит пробелы и обрезает текст до limit байт по границе руны.
func excerpt(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "..."
}

func (s *Server) writeSwarmRequest(w http.ResponseWriter, r *http.Request, req store.SwarmRequest, reason string) {
	views, err := s.swarmRequestViews(r.Context(), req.SwarmID, []store.SwarmRequest{req})
	if err != nil {
		swarmRequestError(w, r, err)
		return
	}
	views[0].DeliveryError = reason
	writeJSON(w, http.StatusOK, views[0])
}

// swarmRequestViews рисует запросы роя: узел шага, имя актора и автора
// ответа берутся из текущих строк; исчезнувший актор остаётся без имени.
func (s *Server) swarmRequestViews(ctx context.Context, swarmID string, requests []store.SwarmRequest) ([]swarmRequestJSON, error) {
	steps, err := s.store.SwarmSteps(ctx, swarmID)
	if err != nil {
		return nil, err
	}
	nodes := map[int64]string{}
	for _, v := range steps {
		nodes[v.Sequence] = v.NodeID
	}
	actors, users := map[string]string{}, map[string]string{}
	out := make([]swarmRequestJSON, 0, len(requests))
	for _, req := range requests {
		v := swarmRequestJSON{ID: req.ID, SwarmID: req.SwarmID, Origin: req.Origin, Sequence: req.Sequence, Kind: req.Kind, Question: req.Question, Context: req.Context, Options: []swarmRequestOptionJSON{}, State: req.State, CreatedAt: req.CreatedAt, AnsweredAt: req.AnsweredAt, Outcome: req.Outcome, Response: req.Response, Delivery: req.Delivery}
		if req.Sequence != nil {
			v.NodeID = nodes[*req.Sequence]
		}
		for _, o := range req.Options {
			v.Options = append(v.Options, swarmRequestOptionJSON{Label: o.Label, Outcome: o.Outcome})
		}
		if label, ok := req.OptionLabel(); ok {
			v.OptionLabel = label
		}
		if req.ActorUID != "" {
			name, seen := actors[req.ActorUID]
			if !seen {
				if a, err := s.store.GetManagedActor(ctx, req.ActorUID); err == nil && a.SwarmID == swarmID {
					name = a.Name
				} else if err != nil && !errors.Is(err, store.ErrNotFound) {
					return nil, err
				}
				actors[req.ActorUID] = name
			}
			v.Actor = &swarmRequestActorJSON{Name: name, Incarnation: req.ActorIncarnation}
		}
		if req.AnsweredBy != "" {
			name, seen := users[req.AnsweredBy]
			if !seen {
				u, err := s.store.GetUser(ctx, req.AnsweredBy)
				if err != nil && !errors.Is(err, store.ErrNotFound) {
					return nil, err
				}
				name = u.Username
				users[req.AnsweredBy] = name
			}
			v.AnsweredBy = name
		}
		out = append(out, v)
	}
	return out, nil
}

// deliveryLocks сериализует доставку одного ответа внутри процесса.
// Нулевое значение готово к работе; запись удаляется с последним ждущим.
type deliveryLocks struct {
	mu    sync.Mutex
	locks map[string]*deliveryLock
}

type deliveryLock struct {
	sync.Mutex
	waiters int
}

func (l *deliveryLocks) lock(id string) func() {
	l.mu.Lock()
	if l.locks == nil {
		l.locks = map[string]*deliveryLock{}
	}
	entry := l.locks[id]
	if entry == nil {
		entry = &deliveryLock{}
		l.locks[id] = entry
	}
	entry.waiters++
	l.mu.Unlock()
	entry.Lock()
	return func() {
		entry.Unlock()
		l.mu.Lock()
		entry.waiters--
		if entry.waiters == 0 {
			delete(l.locks, id)
		}
		l.mu.Unlock()
	}
}
