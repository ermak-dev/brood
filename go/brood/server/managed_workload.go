package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/auth"
	"brood.dev/go/brood/gitorigin"
	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/store"
)

type managedWorkloadOperation struct {
	server       *Server
	context      context.Context
	swarm        app.Swarm
	pool         store.ManagedPool
	name, action string
	actor        store.ManagedActor
	saved        store.ManagedActor
	userID       string
	role         string
	delegated    bool
	parent       *store.ManagedActor
	origin       string
	// intent сохраняется до commit и запуска PVE отдельно от saved: запись
	// задачи может вернуть пустой actor, но не отменяет принятое намерение.
	intent store.ManagedActor
}

func (s *Server) shardReservations(ctx context.Context, swarms []app.Swarm) ([]app.ActorReservation, error) {
	out := []app.ActorReservation{}
	for _, sw := range swarms {
		if sw.State == app.StateDeleted {
			continue
		}
		rows, err := s.store.ListManagedActors(ctx, sw.ID)
		if err != nil {
			return nil, err
		}
		for _, a := range rows {
			if a.Phase == "deleted" {
				continue
			}
			out = append(out, app.ActorReservation{SwarmID: sw.ID, Name: a.Name, CPUMillicores: a.Resources.CPUMillicores, MemoryBytes: a.Resources.MemoryBytes})
		}
	}
	return out, nil
}

func (o *managedWorkloadOperation) authorize() error {
	// Namespace — сетевое чтение: полномочия могли отозвать, пока оно шло.
	userID := o.userID
	if userID == "" {
		user, ok := auth.UserFromContext(o.context)
		if !ok {
			return app.ErrNotAllowed
		}
		userID = user.ID
	}
	if o.delegated {
		u, err := o.server.store.GetUser(o.context, userID)
		if err != nil || !u.IsAdmin || u.Disabled {
			return app.ErrNotAllowed
		}
	}
	if o.parent != nil {
		if err := o.authorizeParent(o.context); err != nil {
			return err
		}
	}
	allowed, err := o.server.app.Can(userID, app.ActionManageSwarm, app.SwarmRes(o.swarm.ID))
	if err != nil {
		return err
	}
	if !allowed {
		return app.ErrNotAllowed
	}
	if !commandSessionValid(o.context) {
		return app.ErrNotAllowed
	}
	return o.context.Err()
}

func (o *managedWorkloadOperation) authorizeParent(ctx context.Context) error {
	if o.parent == nil {
		return nil
	}
	current, _, err := o.server.managed.authorizeCommandActorDurable(ctx, *o.parent)
	if err != nil {
		return err
	}
	permission := o.action
	switch permission {
	case "create":
		permission = "add"
	case "delete":
		permission = "rm"
	}
	return o.server.managed.authorizeGuestPermission(ctx, current, permission)
}

// authorizeIntent проверяет права по тому же snapshot, в котором сохраняется намерение.
func (o *managedWorkloadOperation) authorizeIntent(ctx context.Context) error {
	userID := o.userID
	if userID == "" {
		user, ok := auth.UserFromContext(ctx)
		if !ok {
			return app.ErrNotAllowed
		}
		userID = user.ID
	}
	if err := o.authorizeParent(ctx); err != nil {
		return err
	}
	return o.server.authorizeSwarmManagerIntent(ctx, userID, o.swarm.ID)
}

// authorizeSwarmManagerIntent reads management authority in the caller's transaction.
func (s *Server) authorizeSwarmManagerIntent(ctx context.Context, userID, swarmID string) error {
	user, err := s.store.GetUser(ctx, userID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && user.Disabled) {
		return app.ErrNotAllowed
	}
	if err != nil {
		return err
	}
	swarm, err := s.store.GetSwarm(ctx, swarmID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && swarm.State != string(app.StateActive)) {
		return app.ErrNotAllowed
	}
	if err != nil {
		return err
	}
	if !user.IsAdmin && swarm.OwnerID != userID {
		return app.ErrNotAllowed
	}
	if !commandSessionValid(ctx) {
		return app.ErrNotAllowed
	}
	return ctx.Err()
}

// authorizeSwarmOwnerIntent — то же правило: управляет роем его владелец
// или администратор.
func (s *Server) authorizeSwarmOwnerIntent(ctx context.Context, userID, swarmID string) error {
	return s.authorizeSwarmManagerIntent(ctx, userID, swarmID)
}

func (o *managedWorkloadOperation) beginLifecycle(ctx context.Context, a store.ManagedActor) (store.ManagedActor, error) {
	userID := o.userID
	if userID == "" {
		if user, ok := auth.UserFromContext(ctx); ok {
			userID = user.ID
		}
	}
	if o.parent != nil {
		// Мастер управляет своим дочерним воркером в общем порядке шины.
		return o.server.store.BeginManagedActorOperation(ctx, a.UID, a.Revision, store.NewID(), o.action)
	}
	return o.server.store.BeginOwnerManagedActorOperation(ctx, a, store.NewID(), o.action, func(ctx context.Context) error {
		return o.server.authorizeSwarmOwnerIntent(ctx, userID, o.swarm.ID)
	})
}

func (o *managedWorkloadOperation) Prepare(swarms []app.Swarm) (app.WorkloadAdmission, error) {
	plan := app.WorkloadAdmission{Name: o.name}
	if err := o.authorize(); err != nil {
		return plan, err
	}
	if o.action == "stop" || o.action == "delete" || o.action == "reset" {
		plan.NoAllocation = true
		return plan, nil
	}
	if !o.server.managed.poolAvailable(o.pool) {
		return plan, fmt.Errorf("%w: the Proxmox connection of this pool is not configured or changed; an administrator should check the Pools page", app.ErrConflict)
	}
	if o.pool.State != "enabled" {
		return plan, fmt.Errorf("%w: the pool is being drained and takes no new agents; choose another pool or ask an administrator to enable it", app.ErrConflict)
	}
	resources := o.actor.Resources
	if o.action == "create" {
		var err error
		resources, err = managedActorResources(o.pool)
		if err != nil {
			return plan, err
		}
	}
	plan.CPUMillicores = resources.CPUMillicores
	plan.MemoryBytes = resources.MemoryBytes
	plan.Replace = o.action != "create"
	var err error
	plan.Reservations, err = o.server.shardReservations(o.context, swarms)
	return plan, err
}

func (o *managedWorkloadOperation) Apply() error {
	if err := o.authorize(); err != nil {
		return err
	}
	m := o.server.managed
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	client, err := m.client(o.pool)
	if err != nil {
		return err
	}
	var a store.ManagedActor
	if o.action == "create" {
		if m.hub == nil {
			return fmt.Errorf("%w: the server cannot accept agent connections: BROOD_PUBLIC_ORIGIN and BROOD_PROXMOX_RELAY_SECRET must be set", app.ErrConflict)
		}
		id, err := client.NextID(o.context)
		if err != nil {
			return err
		}
		resources, err := managedActorResources(o.pool)
		if err != nil {
			return err
		}
		if err := o.authorize(); err != nil {
			return err
		}
		delegator, parentUID := "", ""
		if o.delegated {
			delegator = o.userID
		}
		if o.parent != nil {
			parentUID = o.parent.UID
		}
		template, err := m.imageTemplate(o.context, o.swarm.ProjectID, o.pool)
		if err != nil {
			return err
		}
		a, err = o.server.reserveManagedActorOrigin(o.context, store.ManagedActor{Template: template, Role: o.role, DelegatorID: delegator, ParentUID: parentUID, UID: store.NewID(), Name: o.name, SwarmID: o.swarm.ID, PoolUID: o.pool.UID, Resources: resources, CTID: id, OwnerNonce: store.NewID(), OperationID: store.NewID(), Origin: o.origin})
		if err != nil {
			return err
		}
	} else {
		a, err = o.server.store.GetManagedActor(o.context, o.actor.UID)
		if err != nil {
			return err
		}
		if a.Revision != o.actor.Revision {
			return store.ErrRevisionConflict
		}
		if o.action == "start" && a.Stage == "done" && a.Phase == "running" && a.Desired == "running" {
			o.saved = a
			return nil
		}
		if ((o.action == "stop" && a.Desired == "stopped") || (o.action == "delete" && a.Desired == "deleted")) && !(a.Stage == "done" && a.Phase == "error") {
			o.saved = a
			return nil
		}
		// Start, ещё не запрошенный (#133, #275), задачи не держит: CT
		// остановлена, и stop, delete и reset его отменяют. Start
		// ничего бы не добавил к уже записанному start: отказ называет
		// ожидание, а не неизвестный исход. Незапрошенные stop и delete
		// (#277) тоже задачи не держат, но их изменение уже идёт, и любое
		// другое ждёт саму остановку или удаление.
		waiting := startNotRequested(a)
		cancels := o.action == "stop" || o.action == "delete" || o.action == "reset"
		if notRequested(a) && !(waiting && cancels) {
			return newWaitingRefusal(o.action, a)
		}
		// Stop/start/reset require a reconciled task slot, not merely a terminal
		// diagnostic phase. Delete may settle a known prior task before cleanup.
		// Создание, на которое PVE не вернул задачу, и контейнера с этим
		// номером нет: удаление снимает запись. Запрос, принятый PVE после
		// потери ответа, оставил бы контейнер без записи, поэтому отсутствие
		// проверяется прямо перед удалением, а не берётся из прошлого прохода.
		unborn := o.action == "delete" && a.Stage == "create" && a.UPID == "" && a.Operation == "create" && client.ConfirmAbsent(o.context, a.CTID) == nil
		if a.Stage != "done" && !waiting && !unborn && (o.action != "delete" || a.UPID == "" || a.Stage == "delete" || a.Operation == "reset") {
			return &app.APIError{Status: http.StatusConflict, Detail: "The previous operation on this agent has no confirmed result in Proxmox, so a new one is not started. If the container does not exist in Proxmox, delete the agent; otherwise wait for Proxmox to finish and retry."}
		}
		if a.Stage != "done" && a.UPID != "" {
			if taskErr := client.WaitTask(o.context, a.UPID); taskErr != nil {
				if !errors.Is(taskErr, proxmox.ErrTaskFailed) {
					return taskErr
				}
				a, err = m.resolveTaskFailure(o.context, client, a)
				if err != nil {
					return err
				}
			}
		}
		observed, err := managedObservedState(o.context, client, a)
		if err != nil {
			return err
		}
		if o.action != "delete" && observed == "absent" {
			return fmt.Errorf("%w: the container of this agent no longer exists in Proxmox; delete the agent and create it again", app.ErrConflict)
		}
		// Пока провайдер отвечает, доступ человека или делегацию могут отозвать.
		// Повторная проверка нужна перед записью намерения, не только до чтения.
		if err := o.authorize(); err != nil {
			return err
		}
		// Avoid publishing a transient running phase for a task-only start that
		// is immediately being deleted. Both local transitions commit together,
		// before submitting the new provider write.
		release, err := m.effects.acquire(o.context, a.UID)
		if err != nil {
			return err
		}
		err = o.server.store.WithTx(o.context, func(ctx context.Context) error {
			// Права, сессия и намерение — один snapshot: отзыв до транзакции
			// запрещает запись даже после успешной внешней проверки.
			if err := o.authorizeIntent(ctx); err != nil {
				return err
			}
			var err error
			// Известная задача уже дождана выше; у незапрошенного start
			// задачи нет вовсе. Слот закрывает наблюдение CT.
			if a.Stage != "done" {
				a, err = o.server.store.ConfirmManagedActorStage(ctx, a.UID, a.Revision, a.OperationID, a.Stage, observed)
				if err != nil {
					return err
				}
			}
			// Завершённый start может ждать ttyd. Явное recovery диагностирует
			// это состояние, а не объявляет гостя работающим без проверки.
			if a.Stage == "done" && a.Phase == "provisioning" {
				a, err = m.saveObservation(ctx, a, "error", "", managedReadinessError)
				if err != nil {
					return err
				}
			}
			switch {
			case o.action == "start" && observed == "running":
				// CT уже работает (#275): ни bootstrap env, который пишется
				// только остановленной CT, ни второй Start ей не нужны.
				// Намерение start сразу согласовано наблюдением, а running
				// опубликует только проверка готовности гостя.
				a, err = o.beginLifecycle(ctx, a)
				if err == nil {
					a, err = m.confirmStartRunning(ctx, a)
				}
			case o.action == "start":
				a, err = o.beginLifecycle(ctx, a)
				if err == nil {
					a, err = m.saveObservation(ctx, a, "stopped", "", "")
				}
			default:
				a, err = o.beginLifecycle(ctx, a)
				if err == nil && a.Stage == "stop" && observed == "stopped" {
					// Эта stop-стадия создана в текущей транзакции и ещё не
					// отправлена провайдеру. Владение и stopped уже
					// проверены: остановка подтверждена без vzstop, на
					// который PVE ответил бы «CT not running» (#275). Stop
					// согласуется сразу, delete и reset переходят к delete.
					// Для старой неизвестной отправки наблюдение stopped не
					// доказывает исход.
					a, err = o.server.store.ConfirmManagedActorStage(ctx, a.UID, a.Revision, a.OperationID, a.Stage, observed)
				}
			}
			if err == nil {
				o.intent = a
			}
			return err
		})
		release()
		if err != nil {
			if errors.Is(err, store.ErrPoolUnavailable) || errors.Is(err, store.ErrRevisionConflict) || errors.Is(err, app.ErrNotAllowed) {
				return err
			}
			// Отказ брони называет идущую операцию над workspace: общий
			// текст отправил бы сверять задачу провайдера.
			var held *store.CoordinationConflictError
			if errors.As(err, &held) {
				return fmt.Errorf("%w: %w", app.ErrConflict, held)
			}
			return &app.APIError{Status: http.StatusConflict, Detail: "The agent changed while the request was processed. Reload the list and retry."}
		}
		if o.action == "stop" || o.action == "delete" || o.action == "reset" {
			if m.hub != nil {
				m.hub.Disconnect(a.UID, a.Incarnation)
			}
			m.verified.Delete(a.UID)
			o.server.proxy.RecheckNow()
		}
		if o.action == "delete" && observed == "absent" {
			// No stop/delete was submitted: prior task is terminal and the exact
			// VMID was positively proven free. There is no task to await here.
			// The stage closes with the release in one transaction (#277).
			a, err = m.cleanupAbsent(o.context, client, a)
			o.saved = a
			return err
		}
		// Stop остановленной CT и start работающей согласованы без задачи.
		if a.Stage == "done" {
			o.saved = a
			return nil
		}
	}
	a, err = m.launch(o.context, client, a)
	o.saved = a
	return err
}

func (s *Server) createManagedActor(w http.ResponseWriter, r *http.Request, userID string, sw app.Swarm, name string, pool store.ManagedPool, creation actorCreation) {
	a, err := s.createManaged(r.Context(), userID, sw, name, pool, creation.role, creation.origin, nil)
	if err != nil {
		managedAPIError(w, r, err)
		return
	}
	writeJSON(w, 201, s.managedActorRoleJSON(r.Context(), sw, a, pool, s.settledOriginMode(r.Context(), sw.ProjectID, a.Origin)))
}

// createManaged is shared by human creation and UID-bound delegated creation.
// A delegated name retry returns its original reservation without provider writes;
// a retry naming a different origin conflicts. Origin resolution precedes admission.
func (s *Server) createManaged(ctx context.Context, userID string, sw app.Swarm, name string, pool store.ManagedPool, role, origin string, parent *store.ManagedActor) (store.ManagedActor, error) {
	if name == telegramOperator {
		// Имя operator на шине занято владельцем роя.
		return store.ManagedActor{}, &app.APIError{Status: 400, Detail: "The name operator is reserved for the human owner of the swarm. Choose another name."}
	}
	op := &managedWorkloadOperation{server: s, context: ctx, userID: userID, swarm: sw, pool: pool, name: name, action: "create", role: role, parent: parent}
	s.managed.admission.Lock()
	defer s.managed.admission.Unlock()
	assigned, err := s.store.GetAgentRole(ctx, role)
	if errors.Is(err, store.ErrNotFound) {
		return store.ManagedActor{}, &app.APIError{Status: 400, Detail: "Select a role from the list; no agent was created"}
	}
	if err != nil {
		return store.ManagedActor{}, err
	}
	op.role, op.delegated = assigned.ID, assigned.RequiresDelegation()
	if op.delegated && parent != nil {
		return store.ManagedActor{}, app.ErrNotAllowed
	}
	if err := op.authorize(); err != nil {
		return store.ManagedActor{}, err
	}
	if parent != nil {
		a, err := s.store.LookupManagedActor(ctx, sw.ID, op.name)
		if err == nil {
			if a.ParentUID != parent.UID || a.PoolUID != pool.UID {
				return store.ManagedActor{}, app.ErrExists
			}
			if origin != "" {
				canonical, err := gitorigin.Canonical(origin)
				if err != nil {
					return store.ManagedActor{}, &originRefusal{detail: err.Error()}
				}
				if canonical != a.Origin {
					return store.ManagedActor{}, app.ErrExists
				}
			}
			return a, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return store.ManagedActor{}, err
		}
	}
	resolved, err := s.resolveActorOrigin(ctx, sw.ProjectID, origin, parent)
	if err != nil {
		return store.ManagedActor{}, err
	}
	op.origin = resolved
	err = s.app.AdmitWorkload(userID, sw.ID, op)
	return op.saved, err
}

func (s *Server) handleManagedActor(w http.ResponseWriter, r *http.Request, userID string, sw app.Swarm, a store.ManagedActor) {
	p, err := s.store.GetManagedPool(r.Context(), a.PoolUID)
	if err != nil {
		managedAPIError(w, r, err)
		return
	}
	if r.Method == http.MethodGet {
		mode, err := s.originMode(r.Context(), sw.ProjectID, a.Origin)
		if err != nil {
			managedAPIError(w, r, err)
			return
		}
		writeJSON(w, 200, s.managedActorRoleJSON(r.Context(), sw, a, p, mode))
		return
	}
	if r.Method == http.MethodPut {
		writeDetail(w, 409, "an agent cannot be edited; create a new one")
		return
	}
	action := "delete"
	if r.Method == http.MethodPost {
		var ok bool
		action, ok = readAction(w, r, "resume", "stop", "reset", "suspend")
		if !ok {
			return
		}
		if action == "suspend" {
			writeDetail(w, 409, "suspend is not supported; stop the agent instead, its workspace is kept")
			return
		}
		if action == "resume" {
			action = "start"
		}
	}
	op := &managedWorkloadOperation{server: s, context: r.Context(), swarm: sw, pool: p, name: a.Name, action: action, actor: a}
	if err := s.admitWorkload(userID, sw.ID, op); err != nil {
		managedAPIError(w, r, err)
		return
	}
	if r.Method == http.MethodDelete {
		w.WriteHeader(202)
		return
	}
	writeJSON(w, 200, s.managedActorRoleJSON(r.Context(), sw, op.saved, p, s.settledOriginMode(r.Context(), sw.ProjectID, op.saved.Origin)))
}
