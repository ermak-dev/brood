package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"brood.dev/go/brood/pipeline"
)

// ErrSwarmRequestDenied: пользователь не вправе отвечать на запросы роя.
// Проверка идёт в той же транзакции, что и запись ответа.
var ErrSwarmRequestDenied = errors.New("swarm request access denied")

// ErrSwarmRequestStale: актор запроса больше не тот, что спрашивал.
var ErrSwarmRequestStale = errors.New("swarm request actor is stale")

// ErrSwarmRequestInvalid: содержимое запроса или ответа нарушает правила.
var ErrSwarmRequestInvalid = errors.New("invalid swarm request")

// ErrSwarmRequestLimit: у актора уже открыт предельный набор вопросов.
// Идёт вместе с ErrSwarmRequestInvalid; отдельный признак нужен, чтобы
// гость получил причину отказа фиксированным текстом (#210).
var ErrSwarmRequestLimit = errors.New("too many open swarm requests")

const (
	// MaxSwarmRequestTextBytes ограничивает вопрос, материал и ответ.
	MaxSwarmRequestTextBytes = 16384
	maxSwarmRequestOptions   = 16
	// maxOpenActorRequests ограничивает открытые вопросы одного актора:
	// без предела актор забивает оператору очередь.
	maxOpenActorRequests = 16
)

// RequestOption — вариант выбора: подпись для человека и маршрутный исход.
type RequestOption struct {
	Label   string `json:"label"`
	Outcome string `json:"outcome"`
}

// SwarmRequest — запрос оператору и (после ответа) его ответ.
type SwarmRequest struct {
	ID, SwarmID      string
	Sequence         *int64
	Origin           string
	ActorUID         string
	ActorIncarnation int64
	Kind             string
	Question         string
	Context          string
	Options          []RequestOption
	State            string
	CreatedAt        string
	AnsweredAt       *string
	AnsweredBy       string
	// Outcome — маршрутная часть ответа; у origin='actor' всегда пуста.
	Outcome *string
	// Response — свободный текст; в выборе ребра не участвует.
	Response *string
	// Delivery — доставка ответа спросившему актору (#210): pending,
	// delivered или failed у отвеченного запроса актора, иначе пусто.
	Delivery string
}

// OptionLabel восстанавливает подпись варианта по сохранённому исходу.
func (r SwarmRequest) OptionLabel() (string, bool) {
	if r.Outcome == nil {
		return "", false
	}
	for _, o := range r.Options {
		if o.Outcome == *r.Outcome {
			return o.Label, true
		}
	}
	return "", false
}

const swarmRequestCols = `id,swarm_id,sequence,origin,COALESCE(actor_uid,''),COALESCE(actor_incarnation,0),kind,question,COALESCE(context,''),options,state,created_at,answered_at,COALESCE(answered_by,''),outcome,response,COALESCE(delivery,'')`

func scanSwarmRequest(row interface{ Scan(...any) error }) (SwarmRequest, error) {
	var r SwarmRequest
	var options []byte
	err := row.Scan(&r.ID, &r.SwarmID, &r.Sequence, &r.Origin, &r.ActorUID, &r.ActorIncarnation, &r.Kind, &r.Question, &r.Context, &options, &r.State, &r.CreatedAt, &r.AnsweredAt, &r.AnsweredBy, &r.Outcome, &r.Response, &r.Delivery)
	if err != nil {
		return r, err
	}
	if len(options) > 0 {
		if err := json.Unmarshal(options, &r.Options); err != nil {
			return r, fmt.Errorf("decode request options: %w", err)
		}
	}
	return r, nil
}

// SwarmRequests возвращает запросы роя в порядке создания.
func (s *Store) SwarmRequests(ctx context.Context, swarmID string) ([]SwarmRequest, error) {
	if !validPipelineField(swarmID) {
		return nil, errors.New("invalid swarm ID")
	}
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT `+swarmRequestCols+` FROM swarm_requests WHERE swarm_id=? ORDER BY created_at,id`, swarmID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []SwarmRequest{}
	for rows.Next() {
		r, err := scanSwarmRequest(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// GetSwarmRequest возвращает запрос своего роя; чужой ID — ErrNotFound.
func (s *Store) GetSwarmRequest(ctx context.Context, swarmID, id string) (SwarmRequest, error) {
	r, err := scanSwarmRequest(s.conn(ctx).QueryRowContext(ctx, `SELECT `+swarmRequestCols+` FROM swarm_requests WHERE swarm_id=? AND id=?`, swarmID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, fmt.Errorf("%w: swarm request %q", ErrNotFound, id)
	}
	return r, err
}

// openNodeRequest заводит запрос человеческого шага. Вызывается в
// транзакции, которая создаёт посещение в состоянии waiting: шаг и запрос
// появляются вместе или не появляются. material — то, что оператор
// одобряет: отчёт шага, который ввёл граф в узел (#233); пустой — без
// материала. Та же транзакция ставит уведомление request_opened (#282).
func (s *Store) openNodeRequest(ctx context.Context, swarmID string, sequence int64, node string, n pipeline.Node, material string) error {
	options := make([]RequestOption, 0, len(n.Options))
	for _, o := range n.Options {
		options = append(options, RequestOption{Label: o.Label, Outcome: o.Outcome})
	}
	encoded, err := json.Marshal(options)
	if err != nil {
		return err
	}
	id := NewID()
	if _, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO swarm_requests(id,swarm_id,sequence,origin,kind,question,context,options,state,created_at) VALUES (?,?,?,'node','choice',?,NULLIF(?,''),?,'open',?)`,
		id, swarmID, sequence, n.Question, material, encoded, nowRFC3339()); err != nil {
		return err
	}
	return s.queueTelegram(ctx, "request_opened", TelegramEvent{Swarm: swarmID, Request: id, Sequence: sequence, Node: node})
}

// ActorRequestInput — вопрос актора по его инициативе. Личность актора
// проверил вызывающий код; хранилище сверяет принадлежность рою и его
// atespace, инкарнацию и жизнь актора по правилу привязки роли (#208).
type ActorRequestInput struct {
	SwarmID, ActorUID string
	ActorIncarnation  int64
	Kind              string
	Question, Context string
	Options           []RequestOption
}

// OpenActorRequest заводит вопрос актора. Он не связан с шагом графа и
// граф не продвигает. Ответ на него закрывает вопрос, но не меняет
// ничьих полномочий.
func (s *Store) OpenActorRequest(ctx context.Context, in ActorRequestInput) (SwarmRequest, error) {
	if !validPipelineField(in.SwarmID) || !validPipelineField(in.ActorUID) || in.ActorIncarnation < 1 {
		return SwarmRequest{}, fmt.Errorf("%w: actor identity", ErrSwarmRequestInvalid)
	}
	if err := validRequestText(in.Question, true); err != nil {
		return SwarmRequest{}, err
	}
	if err := validRequestText(in.Context, false); err != nil {
		return SwarmRequest{}, err
	}
	var encoded []byte
	switch in.Kind {
	case "choice":
		if err := validOptions(in.Options); err != nil {
			return SwarmRequest{}, err
		}
		var err error
		if encoded, err = json.Marshal(in.Options); err != nil {
			return SwarmRequest{}, err
		}
	case "text", "confirm":
		if len(in.Options) != 0 {
			return SwarmRequest{}, fmt.Errorf("%w: options are only for choice", ErrSwarmRequestInvalid)
		}
	default:
		return SwarmRequest{}, fmt.Errorf("%w: kind must be one of: choice, text, confirm", ErrSwarmRequestInvalid)
	}
	id := NewID()
	err := s.WithTx(ctx, func(ctx context.Context) error {
		sw, err := s.GetSwarm(ctx, in.SwarmID)
		if err != nil {
			return err
		}
		if sw.State != "active" {
			return fmt.Errorf("%w: swarm is not active", ErrSwarmRequestDenied)
		}
		// Личность актора сверяется в той же транзакции, что и вставка:
		// UID, рой, atespace, инкарнация и жизненное состояние берутся из
		// актуальной строки. Правило «актор роя, живой» — то же, что у
		// привязки роли (#208): вопрос задаёт только актор, который мог бы
		// занять роль.
		actor, err := s.GetManagedActor(ctx, in.ActorUID)
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("%w: actor does not belong to the swarm", ErrSwarmRequestDenied)
		}
		if err != nil {
			return err
		}
		if err := liveRoleActor(sw, actor); errors.Is(err, ErrSwarmRoleForeignActor) {
			return fmt.Errorf("%w: actor does not belong to the swarm", ErrSwarmRequestDenied)
		} else if err != nil {
			return fmt.Errorf("%w: actor incarnation %d: %v", ErrSwarmRequestStale, in.ActorIncarnation, err)
		}
		if actor.Incarnation != in.ActorIncarnation {
			return fmt.Errorf("%w: actor incarnation %d is not current", ErrSwarmRequestStale, in.ActorIncarnation)
		}
		var open int
		if err := s.conn(ctx).QueryRowContext(ctx, `SELECT count(*) FROM swarm_requests WHERE swarm_id=? AND origin='actor' AND actor_uid=? AND actor_incarnation=? AND state='open'`, in.SwarmID, in.ActorUID, in.ActorIncarnation).Scan(&open); err != nil {
			return err
		}
		if open >= maxOpenActorRequests {
			return fmt.Errorf("%w: %w from this actor", ErrSwarmRequestInvalid, ErrSwarmRequestLimit)
		}
		if _, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO swarm_requests(id,swarm_id,origin,actor_uid,actor_incarnation,kind,question,context,options,state,created_at) VALUES (?,?,'actor',?,?,?,?,NULLIF(?,''),?,'open',?)`,
			id, in.SwarmID, in.ActorUID, in.ActorIncarnation, in.Kind, in.Question, in.Context, encoded, nowRFC3339()); err != nil {
			return err
		}
		return s.queueTelegram(ctx, "actor_question", TelegramEvent{Swarm: in.SwarmID, Request: id})
	})
	if err != nil {
		return SwarmRequest{}, err
	}
	return s.GetSwarmRequest(ctx, in.SwarmID, id)
}

// actorAnswerable: ответ на уже заданный вопрос имеет адресата, пока актор
// той же инкарнации не удалён и не удаляется.
func actorAnswerable(phase, desired string) bool {
	return phase != "deleted" && phase != "deleting" && desired != "deleted"
}

// requireCurrentActor отвергает ответ на вопрос актора, которого уже нет:
// нет строки, чужой рой или atespace (то же правило принадлежности, что у
// привязки роли, #208), новая инкарнация или удаление. Остановленный актор
// своего роя ответ получает: доставка от ответа не зависит. Ничего не пишет.
func (s *Store) requireCurrentActor(ctx context.Context, w Swarm, req SwarmRequest) error {
	a, err := s.GetManagedActor(ctx, req.ActorUID)
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("%w: actor is gone", ErrSwarmRequestStale)
	}
	if err != nil {
		return err
	}
	if err := liveRoleActor(w, a); errors.Is(err, ErrSwarmRoleForeignActor) {
		return fmt.Errorf("%w: actor no longer belongs to the swarm", ErrSwarmRequestStale)
	}
	if a.Incarnation != req.ActorIncarnation || !actorAnswerable(a.Phase, a.Desired) {
		return fmt.Errorf("%w: actor incarnation %d is %s/%s now", ErrSwarmRequestStale, req.ActorIncarnation, a.Phase, a.Desired)
	}
	return nil
}

func validRequestText(text string, required bool) error {
	if required && strings.TrimSpace(text) == "" {
		return fmt.Errorf("%w: text is required", ErrSwarmRequestInvalid)
	}
	if len(text) > MaxSwarmRequestTextBytes {
		return fmt.Errorf("%w: text exceeds %d bytes", ErrSwarmRequestInvalid, MaxSwarmRequestTextBytes)
	}
	return nil
}

func validOptions(options []RequestOption) error {
	if len(options) == 0 || len(options) > maxSwarmRequestOptions {
		return fmt.Errorf("%w: choice needs 1 to %d options", ErrSwarmRequestInvalid, maxSwarmRequestOptions)
	}
	seen := map[string]bool{}
	for _, o := range options {
		if !validPipelineField(o.Label) || !validPipelineField(o.Outcome) || seen[o.Outcome] {
			return fmt.Errorf("%w: option labels and outcomes must be unique and non-empty", ErrSwarmRequestInvalid)
		}
		seen[o.Outcome] = true
	}
	return nil
}

// SwarmAnswer — ответ оператора. Outcome обязателен у узлового запроса и
// запрещён у запроса актора; Response — свободный текст.
type SwarmAnswer struct {
	SwarmID, RequestID, UserID string
	Outcome, Response          string
}

// AnswerSwarmRequest записывает ответ одним действием с его последствиями.
// authorize проверяет сессию вызывающего контекстом транзакции, затем
// хранилище перепроверяет полномочия пользователя (активный, участник
// проекта, owner или editor живого роя) там же, поэтому отзыв прав между
// открытием запроса и ответом отвергает ответ. Ответ на уже отвеченный или
// отменённый запрос — ErrPipelineVisitConflict, и из двух параллельных
// ответов проходит один. Запись ответа ролей и членства не меняет.
// Доставка ответа актору здесь не происходит: сохранённый ответ не зависит
// от успеха доставки и повторяется независимо.
func (s *Store) AnswerSwarmRequest(ctx context.Context, in SwarmAnswer, authorize func(context.Context) bool) (SwarmRequest, error) {
	if !validPipelineField(in.SwarmID) || !validPipelineField(in.RequestID) {
		return SwarmRequest{}, ErrNotFound
	}
	if err := validRequestText(in.Response, false); err != nil {
		return SwarmRequest{}, err
	}
	err := s.WithTx(ctx, func(ctx context.Context) error {
		if authorize == nil || !authorize(ctx) {
			return ErrSwarmRequestDenied
		}
		w, err := s.requireSwarmManager(ctx, in.SwarmID, in.UserID)
		if err != nil {
			return err
		}
		req, err := s.GetSwarmRequest(ctx, in.SwarmID, in.RequestID)
		if err != nil {
			return err
		}
		if req.State == "answered" && sameAnswer(req, in) {
			// Потерянный ответ сервера: автор повторяет свой применённый
			// ответ. Права и сессия уже проверены выше; ничего не пишем.
			return nil
		}
		if req.State != "open" {
			return ErrPipelineVisitConflict
		}
		if err := validateAnswer(req, in); err != nil {
			return err
		}
		if req.Origin == "actor" {
			if err := s.requireCurrentActor(ctx, w, req); err != nil {
				return err
			}
		}
		if err := s.recordAnswer(ctx, req, in.UserID, in.Outcome, in.Response); err != nil {
			return err
		}
		// Ответ из панели, CLI или Telegram — одно событие (#282), и оно
		// идёт раньше конца прохода или следующего запроса, которые
		// открывает продвижение графа.
		answered := TelegramEvent{Swarm: req.SwarmID, Request: req.ID}
		if req.Sequence != nil {
			answered.Sequence = *req.Sequence
			if answered.Node, err = s.stepNode(ctx, req.SwarmID, *req.Sequence); err != nil {
				return err
			}
		}
		if err := s.queueTelegram(ctx, "request_answered", answered); err != nil {
			return err
		}
		if req.Origin != "node" {
			return nil
		}
		return s.advanceNodeRequest(ctx, w, req, in.Outcome)
	})
	if err != nil {
		return SwarmRequest{}, err
	}
	return s.GetSwarmRequest(ctx, in.SwarmID, in.RequestID)
}

// sameAnswer: повтор принимается только от автора ответа с тем же исходом
// и тем же текстом; любое отличие остаётся конфликтом.
func sameAnswer(req SwarmRequest, in SwarmAnswer) bool {
	outcome, response := "", ""
	if req.Outcome != nil {
		outcome = *req.Outcome
	}
	if req.Response != nil {
		response = *req.Response
	}
	return req.AnsweredBy != "" && req.AnsweredBy == in.UserID && outcome == in.Outcome && response == in.Response
}

// requireSwarmManager сверяет в транзакции, что пользователь вправе
// управлять роем: активный, живой рой, админ либо участник проекта с ролью
// owner или editor.
func (s *Store) requireSwarmManager(ctx context.Context, swarmID, userID string) (Swarm, error) {
	if !validPipelineField(userID) {
		return Swarm{}, ErrSwarmRequestDenied
	}
	user, err := s.GetUser(ctx, userID)
	if err != nil || user.Disabled {
		return Swarm{}, ErrSwarmRequestDenied
	}
	w, err := s.GetSwarm(ctx, swarmID)
	if err != nil || w.State != "active" {
		return Swarm{}, ErrSwarmRequestDenied
	}
	if user.IsAdmin {
		return w, nil
	}
	member, err := s.IsMember(ctx, w.ProjectID, userID)
	if err != nil {
		return Swarm{}, err
	}
	if !member {
		return Swarm{}, ErrSwarmRequestDenied
	}
	role, err := s.GetSwarmRole(ctx, swarmID, userID)
	if errors.Is(err, ErrNotFound) {
		return Swarm{}, ErrSwarmRequestDenied
	}
	if err != nil {
		return Swarm{}, err
	}
	if role != "owner" && role != "editor" {
		return Swarm{}, ErrSwarmRequestDenied
	}
	return w, nil
}

// validateAnswer проверяет ответ по правилам происхождения запроса.
func validateAnswer(req SwarmRequest, in SwarmAnswer) error {
	if req.Origin == "node" {
		for _, o := range req.Options {
			if o.Outcome == in.Outcome && in.Outcome != "" {
				return nil
			}
		}
		return fmt.Errorf("%w: outcome is not one of the request options", ErrSwarmRequestInvalid)
	}
	if in.Outcome != "" {
		return fmt.Errorf("%w: an actor request carries no route outcome", ErrSwarmRequestInvalid)
	}
	switch req.Kind {
	case "text":
		if strings.TrimSpace(in.Response) == "" {
			return fmt.Errorf("%w: response is required", ErrSwarmRequestInvalid)
		}
	case "confirm":
		if in.Response != "yes" && in.Response != "no" {
			return fmt.Errorf("%w: confirm response must be yes or no", ErrSwarmRequestInvalid)
		}
	case "choice":
		for _, o := range req.Options {
			if o.Label == in.Response {
				return nil
			}
		}
		return fmt.Errorf("%w: response must be one of the option labels", ErrSwarmRequestInvalid)
	}
	return nil
}

// advanceNodeRequest продвигает граф по исходу узлового ответа. Только
// исход попадает в движок: свободный текст сюда не передаётся.
func (s *Store) advanceNodeRequest(ctx context.Context, w Swarm, req SwarmRequest, outcome string) error {
	if !activeExecution(w.Execution) || req.Sequence == nil {
		return ErrPipelineVisitConflict
	}
	var node, state string
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT node_id,state FROM swarm_steps WHERE swarm_id=? AND sequence=?`, w.ID, *req.Sequence).Scan(&node, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrPipelineVisitConflict
	}
	if err != nil {
		return err
	}
	if state != "waiting" {
		return ErrPipelineVisitConflict
	}
	d, err := pipeline.Parse(string(w.Graph), "pipeline")
	if err != nil {
		return err
	}
	if n, ok := d.Nodes[node]; !ok || n.Type != "human" {
		return errors.New("visit is not a human node")
	}
	return s.progressPipeline(ctx, w, d, *req.Sequence, node, "answered", outcome, "")
}

// CancelSwarmRequest отменяет открытый запрос. Отмена узлового запроса
// остановила бы граф без исхода, поэтому разрешена только запросу актора;
// узловые запросы закрывает остановка исполнения роя.
func (s *Store) CancelSwarmRequest(ctx context.Context, swarmID, id, userID string, authorize func(context.Context) bool) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		if authorize == nil || !authorize(ctx) {
			return ErrSwarmRequestDenied
		}
		if _, err := s.requireSwarmManager(ctx, swarmID, userID); err != nil {
			return err
		}
		req, err := s.GetSwarmRequest(ctx, swarmID, id)
		if err != nil {
			return err
		}
		if req.State != "open" {
			return ErrPipelineVisitConflict
		}
		if req.Origin != "actor" {
			return fmt.Errorf("%w: a node request is closed by stopping the graph", ErrSwarmRequestInvalid)
		}
		_, err = s.conn(ctx).ExecContext(ctx, `UPDATE swarm_requests SET state='cancelled' WHERE swarm_id=? AND id=? AND state='open'`, swarmID, id)
		return err
	})
}

// recordAnswer закрывает открытый запрос ответом. Условие state='open' в
// UPDATE делает запись атомарным compare-and-set; автор обязателен — аудит
// answered_by полный, ответа без пользователя нет: проигравший гонку
// получает ErrPipelineVisitConflict. Ответ актору фиксируется с доставкой
// pending: сама доставка идёт после коммита (#210).
func (s *Store) recordAnswer(ctx context.Context, req SwarmRequest, userID, outcome, response string) error {
	if !validPipelineField(userID) {
		return ErrSwarmRequestDenied
	}
	routed := sql.NullString{String: outcome, Valid: req.Origin == "node"}
	text := sql.NullString{String: response, Valid: response != ""}
	delivery := sql.NullString{String: "pending", Valid: req.Origin == "actor"}
	result, err := s.conn(ctx).ExecContext(ctx, `UPDATE swarm_requests SET state='answered',answered_at=?,answered_by=?,outcome=?,response=?,delivery=? WHERE swarm_id=? AND id=? AND state='open'`,
		nowRFC3339(), userID, routed, text, delivery, req.SwarmID, req.ID)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return ErrPipelineVisitConflict
	}
	return nil
}

// RecordSwarmRequestDelivery записывает исход попытки доставки ответа
// спросившему актору (#210): delivered — гость подтвердил приём, failed —
// ответ заведомо не ушёл, unconfirmed — ушёл без квитанции, исход неизвестен
// (#260); другой исход — ErrSwarmRequestInvalid. Доставка идёт после коммита
// ответа, поэтому её сбой не откатывает ответ, а запись исхода не трогает ни
// ответ, ни граф. Пишется только у отвеченного запроса актора с доставкой
// pending, failed или unconfirmed: доставленный ответ не переоткрывается, у
// узлового запроса адресата нет — иначе ErrPipelineVisitConflict. failed
// поверх unconfirmed оставляет unconfirmed: прежняя попытка могла дойти.
// Запрос чужого роя — ErrNotFound. Смена состояния доставки ставит в той же
// транзакции уведомление answer_delivery (#282); исход, который состояния не
// меняет, — нет.
func (s *Store) RecordSwarmRequestDelivery(ctx context.Context, swarmID, id, outcome string) (SwarmRequest, error) {
	if !validPipelineField(swarmID) || !validPipelineField(id) {
		return SwarmRequest{}, ErrNotFound
	}
	switch outcome {
	case "delivered", "failed", "unconfirmed":
	default:
		return SwarmRequest{}, fmt.Errorf("%w: unknown delivery outcome %q", ErrSwarmRequestInvalid, outcome)
	}
	var req SwarmRequest
	err := s.WithTx(ctx, func(ctx context.Context) error {
		before, err := s.GetSwarmRequest(ctx, swarmID, id)
		if err != nil {
			return err
		}
		result, err := s.conn(ctx).ExecContext(ctx, `UPDATE swarm_requests SET delivery=CASE WHEN delivery='unconfirmed' AND ?='failed' THEN 'unconfirmed' ELSE ? END WHERE swarm_id=? AND id=? AND origin='actor' AND state='answered' AND delivery IN ('pending','failed','unconfirmed')`, outcome, outcome, swarmID, id)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			return ErrPipelineVisitConflict
		}
		if req, err = s.GetSwarmRequest(ctx, swarmID, id); err != nil {
			return err
		}
		if req.Delivery == before.Delivery {
			return nil
		}
		return s.queueTelegram(ctx, "answer_delivery", TelegramEvent{Swarm: swarmID, Request: id, Delivery: req.Delivery})
	})
	if err != nil {
		return SwarmRequest{}, err
	}
	return req, nil
}
