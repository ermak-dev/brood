package store

// FEATURE: Запуск и остановка роя (#211).
// SPEC: Запуск делает три вещи в одной транзакции: фиксирует снимок графа с
// личностью определения и источником, проверяет привязки ролей (#221) и
// переводит исполнение в running или waiting вместе с первым шагом
// стартового узла. Остановка из running/waiting замораживает проход; акторы,
// их workspace и привязки остаются. Любой отказ — до записи и с причиной.
// IMPLEMENTATION: StartSwarm и StopSwarm проверяют сессию, рой и право
// управлять им в транзакции записи (requireSwarmManager, #209); первый шаг —
// pipeline.Begin, то же правило входа в узел, что у Advance.
// Снимок графа один на рой и неизменяем после первого запуска (#206,
// swarm_graph_immutable; origin источника — pipeline_sources, тоже один на
// рой). Повторный запуск — новый проход на том же снимке: шаги прошлых
// проходов остаются историей, нумерация продолжается, а узлы старых шагов
// читаются по тому же графу; другое определение — новый рой. Каталог после
// первого запуска не читается: расхождение снимка и каталога — нормальное
// состояние (#218), подменять одно другим нельзя. Источник обязателен с #198,
// repository берётся из проекта. Commit источника закрепляется на проход, а
// не на рой (#229): рой, развивающий проект, начинает каждый проход со
// свежего commit, а акторы и их workspace остаются. Origin остаётся на рой:
// акторы привязаны к нему bootstrap'ом, поэтому repository проекта, отличный
// от origin роя, — отказ, а не проход в чужом repository. Commit каждого
// запуска называет вызывающий, а без него сервер разрешает HEAD repository до
// транзакции (#228): сеть в транзакции записи держала бы блокировку SQLite на
// время upstream. Поэтому транзакция сверяет repository, из которого commit
// разрешён (SwarmStart.Origin), с текущим repository проекта.
// Задача прохода (#242) — текст оператора, с которым идёт проход: она
// пишется в колонку goal первого шага прохода тем же INSERT, что открывает
// проход, и ограждена от правок схемой (swarm_steps_pass_fence). Отдельная
// таблица проходов дала бы второй ключ, внешний ключ к шагу и ещё одну запись
// в транзакции запуска ради одного поля: проход и так — его первый шаг и
// следующие до следующего запуска, номер первого шага его и называет. Колонка
// на каждом шаге копировала бы до 4 КиБ на шаг и потребовала бы переносить её
// в progressPipeline. Шаг находит задачу своего прохода по последнему шагу с
// задачей не дальше себя; шаги идут подряд, потому что новый проход
// начинается только после остановки или завершения прошлого. Commit прохода
// (#229) живёт там же и по той же причине (source_commit): триггерам
// подтверждения хватает подзапроса по (swarm_id, sequence), а копия на каждом
// шаге потребовала бы ещё одной ограды против чужого commit.
// RESULT: IMPLEMENTED — схема 25: swarm_steps.goal, swarm_steps.source_commit
// и swarm_steps_pass_fence.
// STATUS: DONE

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"brood.dev/go/brood/pipeline"
)

// ErrSwarmExecutionState: the swarm or its execution is in a state the
// operation does not start from; the message names the state.
var ErrSwarmExecutionState = errors.New("swarm state conflict")

// ErrSwarmGraphPinned: a restart named another definition than the snapshot
// the swarm was first started with, or the project repository is no longer
// the origin the swarm's actors work in.
var ErrSwarmGraphPinned = errors.New("swarm graph is pinned")

// ErrWorkflowDefinitionMissing: the named definition is not in the catalog
// of the swarm's project.
var ErrWorkflowDefinitionMissing = errors.New("workflow definition is not in the project catalog")

// ErrWorkflowGraphInvalid: the definition does not pass the runner's
// validation: strict parsing, graph rules and supported semantics.
var ErrWorkflowGraphInvalid = errors.New("workflow graph is not executable")

// ErrSwarmSourceInvalid: the start cannot pin the source of its pass: the
// project has no repository or the commit is missing or malformed.
var ErrSwarmSourceInvalid = errors.New("swarm source is invalid")

// ErrSwarmSourceChanged: the project repository changed after the server
// resolved the commit of the start from it.
var ErrSwarmSourceChanged = errors.New("swarm source changed during start")

// SwarmStart is a request to start a new pass of the swarm graph.
type SwarmStart struct {
	SwarmID, UserID string
	// Definition names a definition of the project catalog on the first
	// start; a restart must name the definition of the pinned snapshot.
	Definition string
	// Commit is the full commit of the project repository the new pass runs
	// on (#229); every start requires it, the server resolves it when the
	// caller names none. Passes of one swarm may run on different commits.
	Commit string
	// Origin, when set, is the repository the server resolved Commit from
	// before the transaction (#228). The start refuses with
	// ErrSwarmSourceChanged if the project repository is another by then.
	Origin string
	// Goal is the operator's task for the new pass (#242), required on every
	// start: ValidSwarmGoal. It is kept with the first step of the pass and
	// every step instruction of the pass carries it.
	Goal string
}

// ErrSwarmGoalInvalid: the pass goal is empty or not bounded plain text; the
// message names the reason from a fixed set.
var ErrSwarmGoalInvalid = errors.New("invalid swarm goal")

// MaxSwarmGoalBytes bounds the pass goal; the schema holds the same limit.
const MaxSwarmGoalBytes = 4096

// ValidSwarmGoal reports why goal cannot be the goal of a pass: it is empty
// or only whitespace, exceeds MaxSwarmGoalBytes, is not valid UTF-8 or has
// control characters other than newline and tab. The goal is not trimmed or
// otherwise changed; the error is ErrSwarmGoalInvalid with a fixed reason.
func ValidSwarmGoal(goal string) error {
	if strings.TrimSpace(goal) == "" {
		return fmt.Errorf("%w: the goal is empty", ErrSwarmGoalInvalid)
	}
	if problem := plainTextProblem(goal, MaxSwarmGoalBytes); problem != "" {
		return fmt.Errorf("%w: the goal %s", ErrSwarmGoalInvalid, problem)
	}
	return nil
}

// passPlan — проверенный запуск до первой записи.
type passPlan struct {
	doc    *pipeline.Document
	entry  pipeline.Transition
	attach bool
	graph  []byte
	def    WorkflowDefinition
	source *PipelineSource
}

// StartSwarm starts a new pass of the swarm graph and returns the swarm.
// authorize checks the caller's session with the transaction context; the
// user must still manage the active swarm. The swarm must not be running or
// waiting. Every start names the goal of its pass (ValidSwarmGoal); the
// first step of the pass keeps it. The first start takes the definition from
// the catalog of the swarm's project, validates it as the runner would and
// pins it with its identity and the repository origin; a restart reuses the
// pinned snapshot. Every pass pins its own commit (#229). Every role of the
// graph must be bound to a running actor (CheckSwarmRolesFit). All checks
// precede the first write and run in the transaction that writes, so of
// concurrent starts exactly one succeeds. No actor is created or touched.
func (s *Store) StartSwarm(ctx context.Context, in SwarmStart, authorize func(context.Context) bool) (Swarm, error) {
	var started Swarm
	err := s.WithTx(ctx, func(ctx context.Context) error {
		if err := ValidSwarmGoal(in.Goal); err != nil {
			return err
		}
		w, err := s.manageableSwarm(ctx, in.SwarmID, in.UserID, "start", authorize)
		if err != nil {
			return err
		}
		if activeExecution(w.Execution) {
			return fmt.Errorf("%w: execution is %s; start requires idle, succeeded, failed or stopped", ErrSwarmExecutionState, w.Execution)
		}
		plan, err := s.planPass(ctx, w, in)
		if err != nil {
			return err
		}
		if err := s.CheckSwarmRolesFit(ctx, w.ID, plan.doc); err != nil {
			return err
		}
		if plan.attach {
			if err := s.AttachSwarmGraph(ctx, w.ID, plan.graph, plan.def.Name, plan.def.Version, plan.def.Revision, plan.source.Origin); err != nil {
				return err
			}
		}
		if err := s.beginPass(ctx, w.ID, in.Goal, plan); err != nil {
			return err
		}
		started, err = s.GetSwarm(ctx, w.ID)
		return err
	})
	return started, err
}

// StopSwarm freezes the running or waiting pass of the swarm graph. The open
// node request is cancelled by the same statement (swarms_gate_stop, #209),
// so an answer given later cannot advance the graph. The step the pass waits
// on, for delivery, for its workspace attestation or for the operator, is
// closed as stopped: no instruction was sent and none will be. A step whose
// instruction is being delivered becomes unconfirmed, because it may have
// reached the actor; an inflight or unconfirmed step keeps its state, because
// the actor's outcome is unknown, and can no longer complete. Actors, their
// workspaces and role bindings are not touched. Stopping idle, succeeded,
// failed or stopped execution is refused with the state named.
func (s *Store) StopSwarm(ctx context.Context, swarmID, userID string, authorize func(context.Context) bool) (Swarm, error) {
	var stopped Swarm
	err := s.WithTx(ctx, func(ctx context.Context) error {
		w, err := s.manageableSwarm(ctx, swarmID, userID, "stop", authorize)
		if err != nil {
			return err
		}
		if !activeExecution(w.Execution) {
			return fmt.Errorf("%w: execution is %s; stop requires running or waiting", ErrSwarmExecutionState, w.Execution)
		}
		now := nowRFC3339()
		// Момент остановки становится моментом исполнения роя (#268): шаг,
		// остановленный на доставке или после неё, finished_at не получает.
		if _, err := s.conn(ctx).ExecContext(ctx, `UPDATE swarms SET execution='stopped',execution_at=?,updated_at=? WHERE id=?`, now, now, w.ID); err != nil {
			return err
		}
		// Ждущий шаг закрывается здесь, а не при следующем запуске: запуск
		// шагов прошлых проходов не меняет, а ждущий шаг с отменённым
		// запросом не дал бы начать новый проход (swarms_gate_resume).
		// Шаг на подтверждении workspace инструкцию ещё не отправил и уже
		// не отправит: запись подтверждения условна на attesting (#212).
		if _, err := s.conn(ctx).ExecContext(ctx, `UPDATE swarm_steps SET state='stopped',finished_at=? WHERE swarm_id=? AND state IN ('pending','waiting','attesting')`, now, w.ID); err != nil {
			return err
		}
		// Шаг на доставке мог уже уйти актору: исход неизвестен.
		if _, err := s.conn(ctx).ExecContext(ctx, `UPDATE swarm_steps SET state='unconfirmed',detail=? WHERE swarm_id=? AND state='delivering'`, detailStoppedDelivery, w.ID); err != nil {
			return err
		}
		stopped, err = s.GetSwarm(ctx, w.ID)
		return err
	})
	return stopped, err
}

// manageableSwarm — общий вход запуска и остановки: сессия, рой, его
// состояние и право управлять им проверяются в транзакции записи, поэтому
// отзыв прав между проверкой сервера и записью отвергает операцию. Состояние
// названо раньше прав: сервер пускает сюда только видящего рой.
func (s *Store) manageableSwarm(ctx context.Context, swarmID, userID, operation string, authorize func(context.Context) bool) (Swarm, error) {
	if authorize == nil || !authorize(ctx) {
		return Swarm{}, ErrSwarmRequestDenied
	}
	w, err := s.GetSwarm(ctx, swarmID)
	if err != nil {
		return Swarm{}, err
	}
	if w.State != "active" {
		return Swarm{}, fmt.Errorf("%w: swarm is %s; %s requires an active swarm", ErrSwarmExecutionState, w.State, operation)
	}
	if _, err := s.requireSwarmManager(ctx, swarmID, userID); err != nil {
		return Swarm{}, err
	}
	return w, nil
}

// planPass проверяет запуск без записи: первый запуск берёт определение из
// каталога, повторный — привязанный снимок роя; источник прохода — каждый
// раз из repository проекта и названного commit.
func (s *Store) planPass(ctx context.Context, w Swarm, in SwarmStart) (passPlan, error) {
	var plan passPlan
	var err error
	if w.Graph != nil {
		plan, err = pinnedPass(w, in)
	} else {
		plan, err = s.catalogPass(ctx, w, in)
	}
	if err != nil {
		return passPlan{}, err
	}
	if plan.source, err = s.startSource(ctx, w.ProjectID, in); err != nil {
		return passPlan{}, err
	}
	// Акторы роя привязаны к его origin: проход в другом repository
	// подтвердить им нечем.
	if w.Graph != nil && plan.source.Origin != w.Source.Origin {
		return passPlan{}, fmt.Errorf("%w: the swarm actors work in repository %s, the project repository is now %s; start the workflow in another swarm", ErrSwarmGraphPinned, w.Source.Origin, plan.source.Origin)
	}
	if plan.entry, err = pipeline.Begin(plan.doc); err != nil {
		return passPlan{}, fmt.Errorf("%w: %v", ErrWorkflowGraphInvalid, err)
	}
	return plan, nil
}

// catalogPass — первый запуск: определение каталога своего проекта и
// валидатор исполнителя.
func (s *Store) catalogPass(ctx context.Context, w Swarm, in SwarmStart) (passPlan, error) {
	if !validPipelineField(in.Definition) {
		return passPlan{}, fmt.Errorf("%w: a definition name is required", ErrWorkflowDefinitionMissing)
	}
	def, err := s.GetWorkflowDefinition(ctx, w.ProjectID, in.Definition)
	if errors.Is(err, ErrNotFound) {
		return passPlan{}, fmt.Errorf("%w: %q", ErrWorkflowDefinitionMissing, in.Definition)
	}
	if err != nil {
		return passPlan{}, err
	}
	graph := []byte(def.YAML)
	doc, err := executableGraph(graph)
	if err != nil {
		return passPlan{}, fmt.Errorf("%w: workflow %q: %v", ErrWorkflowGraphInvalid, def.Name, err)
	}
	return passPlan{doc: doc, attach: true, graph: graph, def: def}, nil
}

// pinnedPass — повторный запуск на снимке первого: другое определение —
// отказ, а не тихая подмена.
func pinnedPass(w Swarm, in SwarmStart) (passPlan, error) {
	if in.Definition != w.DefinitionName {
		return passPlan{}, fmt.Errorf("%w: the swarm runs workflow %q version %d revision %s; start %q in another swarm", ErrSwarmGraphPinned, w.DefinitionName, w.DefinitionVersion, w.DefinitionRevision, in.Definition)
	}
	// Снимок без источника — только из базы до #198: действия актора в нём
	// не закрыты проверкой checkout, такой граф не запускается.
	if w.Source == nil {
		return passPlan{}, fmt.Errorf("%w: the swarm graph has no source snapshot; start the workflow in a new swarm", ErrSwarmSourceInvalid)
	}
	doc, err := executableGraph(w.Graph)
	if err != nil {
		return passPlan{}, fmt.Errorf("%w: workflow %q: %v", ErrWorkflowGraphInvalid, w.DefinitionName, err)
	}
	return passPlan{doc: doc}, nil
}

// startSource — источник прохода: repository проекта и названный commit. Без repository или без полного commit пиннить нечего.
// Commit, который сервер разрешил до транзакции (#228), принадлежит
// repository in.Origin: сменившийся с тех пор repository проекта — отказ, а не
// снимок из commit одного repository и адреса другого.
func (s *Store) startSource(ctx context.Context, projectID string, in SwarmStart) (*PipelineSource, error) {
	origin, err := s.ProjectRepository(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if origin == "" {
		return nil, fmt.Errorf("%w: the project has no repository to pin the source to", ErrSwarmSourceInvalid)
	}
	if in.Origin != "" && in.Origin != origin {
		return nil, fmt.Errorf("%w: the project repository changed while its branch was resolved; start again", ErrSwarmSourceChanged)
	}
	if in.Commit == "" {
		return nil, fmt.Errorf("%w: the start requires the commit to pin", ErrSwarmSourceInvalid)
	}
	source, err := canonicalPipelineSource(&PipelineSource{Origin: origin, Commit: in.Commit})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSwarmSourceInvalid, err)
	}
	return source, nil
}

// beginPass открывает проход: первый шаг стартового узла получает
// следующий номер роя, задачу и commit прохода, шаги прошлых проходов не
// трогаются.
// Шаг узла-человека
// открывает свой запрос в той же транзакции, как у преемника в
// progressPipeline, но без материала: предыдущий шаг роя принадлежит
// прошлому проходу, и его отчёт не то, что одобряют сейчас (#233). Условие
// на исполнение повторяет проверку запуска в самой записи. Тот же UPDATE
// пишет начало прохода в execution_at (#268): первый шаг вставлен без
// started_at, по шагам момент запуска не восстановить.
func (s *Store) beginPass(ctx context.Context, swarmID, goal string, plan passPlan) error {
	var sequence int64
	if err := s.conn(ctx).QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM swarm_steps WHERE swarm_id=?`, swarmID).Scan(&sequence); err != nil {
		return err
	}
	if _, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO swarm_steps(swarm_id,sequence,node_id,state,goal,source_commit) VALUES (?,?,?,?,?,?)`, swarmID, sequence, plan.entry.Next, plan.entry.NextState, goal, plan.source.Commit); err != nil {
		return err
	}
	if plan.entry.NextState == "waiting" {
		if err := s.openNodeRequest(ctx, swarmID, sequence, plan.doc.Nodes[plan.entry.Next], ""); err != nil {
			return err
		}
	}
	now := nowRFC3339()
	result, err := s.conn(ctx).ExecContext(ctx, `UPDATE swarms SET execution=?,execution_at=?,updated_at=? WHERE id=? AND execution NOT IN ('running','waiting')`, executionOfTransition(plan.entry.Execution), now, now, swarmID)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return fmt.Errorf("%w: execution changed during start", ErrSwarmExecutionState)
	}
	return nil
}

// executableGraph — валидатор графа исполнителя, общий для привязки снимка и
// запуска: предел размера, строгий разбор, правила графа и поддерживаемая
// семантика. Каталог проверяет только разбор и граф, поэтому запуск
// проверяет определение заново.
func executableGraph(graph []byte) (*pipeline.Document, error) {
	if len(graph) == 0 || len(graph) > MaxPipelineGraphBytes {
		return nil, errors.New("workflow graph is empty or too large")
	}
	d, err := pipeline.Parse(string(graph), "pipeline")
	if err != nil {
		return nil, err
	}
	if err := d.Validate(); err != nil {
		return nil, err
	}
	if err := supportedPipeline(d); err != nil {
		return nil, err
	}
	return d, nil
}
