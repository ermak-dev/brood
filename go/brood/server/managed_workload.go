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
	"brood.dev/go/brood/substrate"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
)

type managedWorkloadOperation struct {
	server       *Server
	context      context.Context
	swarm        app.Swarm
	pool         store.ManagedPool
	name, action string
	input        *ateapi.Actor
	actor        store.ManagedActor
	saved        store.ManagedActor
	userID       string
	role         string
	parent       *store.ManagedActor
	origin       string
	// operationID — ID нового намерения; "" — свежий ID при записи. Update
	// (#133) задаёт его заранее, чтобы после сбоя отличить записанное
	// намерение от отказа до него.
	operationID string
	// intent сохраняется до commit и запуска PVE отдельно от saved: запись
	// задачи может вернуть пустой actor, но не отменяет принятое намерение.
	intent store.ManagedActor
}

func (o *managedWorkloadOperation) intentID() string {
	if o.operationID == "" {
		return store.NewID()
	}
	return o.operationID
}

func (s *Server) shardReservations(ctx context.Context, swarms []app.Swarm) ([]app.ActorReservation, error) {
	out, err := nativeShardReservations(ctx, s.ateapi, swarms)
	if err != nil {
		return nil, err
	}
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
			if a.AtespaceUID != sw.AtespaceUID {
				return nil, &shardInventoryError{cause: app.ErrConflict}
			}
			out = append(out, app.ActorReservation{SwarmID: sw.ID, Name: a.Name, CPUMillicores: a.Resources.CPUMillicores, MemoryBytes: a.Resources.MemoryBytes})
		}
	}
	return out, nil
}

func (o *managedWorkloadOperation) authorize() error {
	// Namespace — сетевое чтение: полномочия могли отозвать, пока оно шло.
	if err := o.server.checkSwarmNamespace(o.context, o.swarm); err != nil {
		return err
	}
	userID := o.userID
	if userID == "" {
		user, ok := auth.UserFromContext(o.context)
		if !ok {
			return app.ErrNotAllowed
		}
		userID = user.ID
	}
	if o.role == "master" {
		u, err := o.server.store.GetUser(o.context, userID)
		if err != nil || !u.IsAdmin || u.Disabled {
			return app.ErrNotAllowed
		}
	}
	if o.parent != nil {
		if err := o.server.managed.authorizeMaster(o.context, *o.parent); err != nil {
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

// authorizeIntent проверяет правило manage-swarm по транзакционным чтениям:
// app.Can использует Gateway с context.Background и не видит этот snapshot.
func (o *managedWorkloadOperation) authorizeIntent(ctx context.Context) error {
	userID := o.userID
	if userID == "" {
		user, ok := auth.UserFromContext(ctx)
		if !ok {
			return app.ErrNotAllowed
		}
		userID = user.ID
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
	if !user.IsAdmin {
		member, err := s.store.IsMember(ctx, swarm.ProjectID, userID)
		if err != nil {
			return err
		}
		if !member {
			return app.ErrNotAllowed
		}
		role, err := s.store.GetSwarmRole(ctx, swarm.ID, userID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && role != app.RoleOwner && role != app.RoleEditor) {
			return app.ErrNotAllowed
		}
		if err != nil {
			return err
		}
	}
	if !commandSessionValid(ctx) {
		return app.ErrNotAllowed
	}
	return ctx.Err()
}

func (s *Server) authorizeSwarmOwnerIntent(ctx context.Context, userID, swarmID string) error {
	if err := s.authorizeSwarmManagerIntent(ctx, userID, swarmID); err != nil {
		return err
	}
	user, err := s.store.GetUser(ctx, userID)
	if err != nil {
		return err
	}
	if user.IsAdmin {
		return nil
	}
	role, err := s.store.GetSwarmRole(ctx, swarmID, userID)
	if err != nil || role != app.RoleOwner {
		return app.ErrNotAllowed
	}
	return nil
}

func (o *managedWorkloadOperation) beginLifecycle(ctx context.Context, a store.ManagedActor) (store.ManagedActor, error) {
	userID := o.userID
	if userID == "" {
		if user, ok := auth.UserFromContext(ctx); ok {
			userID = user.ID
		}
	}
	if o.parent != nil {
		return o.server.store.BeginDelegatedManagedActorOperation(ctx, *o.parent, a, o.intentID(), o.action, userID, func(ctx context.Context) error {
			current, err := o.server.store.GetSwarm(ctx, o.swarm.ID)
			if err != nil {
				return err
			}
			if current.ProjectID != o.swarm.ProjectID || current.AtespaceUID == nil || *current.AtespaceUID != o.swarm.AtespaceUID {
				return app.ErrNotAllowed
			}
			return o.server.authorizeSwarmManagerIntent(ctx, userID, o.swarm.ID)
		})
	}
	if o.server.authorizeSwarmOwnerIntent(ctx, userID, o.swarm.ID) == nil {
		return o.server.store.BeginOwnerManagedActorOperation(ctx, a, o.intentID(), o.action, userID, func(ctx context.Context) error {
			return o.server.authorizeSwarmOwnerIntent(ctx, userID, o.swarm.ID)
		})
	}
	return o.server.store.BeginManagedActorOperation(ctx, a.UID, a.Revision, o.intentID(), o.action)
}

func (o *managedWorkloadOperation) Prepare(swarms []app.Swarm) (app.WorkloadAdmission, error) {
	plan := app.WorkloadAdmission{Name: o.name}
	if err := o.authorize(); err != nil {
		return plan, err
	}
	// Update перезапускает уже зарезервированного актора: ёмкость не
	// меняется, доступность пула проверяют клиент и намерение.
	if o.action == "stop" || o.action == "delete" || o.action == "reset" || o.action == "update" {
		plan.TemplateOnly = true
		return plan, nil
	}
	if !o.server.managed.poolAvailable(o.pool) {
		return plan, fmt.Errorf("%w: installed Proxmox runtime unavailable", app.ErrConflict)
	}
	if o.pool.State != "enabled" {
		return plan, fmt.Errorf("%w: managed pool is draining", app.ErrConflict)
	}
	_, nativeErr := o.server.ateapi.GetActor(o.context, o.swarm.AtespaceName, o.name)
	var api *substrate.APIError
	if nativeErr == nil {
		return plan, fmt.Errorf("%w: native actor name is already in use", app.ErrExists)
	}
	if !errors.As(nativeErr, &api) || api.Status != 404 {
		return plan, nativeErr
	}
	_, nativeErr = o.server.ateapi.GetActorTemplate(o.context, o.swarm.AtespaceName, managedTemplatePrefix+o.pool.Name)
	if nativeErr == nil {
		return plan, fmt.Errorf("%w: native template conflicts with managed profile", app.ErrConflict)
	}
	if !errors.As(nativeErr, &api) || api.Status != 404 {
		return plan, nativeErr
	}
	resources := o.actor.Resources
	if o.action == "create" {
		var err error
		resources, err = managedActorResources(o.pool)
		if err != nil {
			return plan, err
		}
		plan.WorkerSelector = o.input.GetWorkerSelector().GetMatchLabels()
	}
	plan.CPUMillicores = resources.CPUMillicores
	plan.MemoryBytes = resources.MemoryBytes
	plan.Replace = o.action != "create"
	var err error
	plan.Reservations, err = o.server.shardReservations(o.context, swarms)
	return plan, err
}

func (o *managedWorkloadOperation) Apply(selector map[string]string) error {
	if err := o.authorize(); err != nil {
		return err
	}
	m := o.server.managed
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	if o.action == "create" || o.action == "start" || o.action == "reset" || o.action == "update" {
		for key, value := range selector {
			if o.pool.Labels[key] != value {
				return fmt.Errorf("%w: managed pool does not match required worker selector", app.ErrInvalidWorkload)
			}
		}
	}
	client, err := m.client(o.pool)
	if err != nil {
		return err
	}
	var a store.ManagedActor
	if o.action == "create" {
		if m.hub == nil {
			return fmt.Errorf("%w: guest relay unavailable", app.ErrConflict)
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
		if o.role == "master" {
			delegator = o.userID
		}
		if o.parent != nil {
			parentUID = o.parent.UID
		}
		a, err = o.server.reserveManagedActorOrigin(o.context, store.ManagedActor{Role: o.role, DelegatorID: delegator, ParentUID: parentUID, UID: store.NewID(), Name: o.name, SwarmID: o.swarm.ID, AtespaceUID: o.swarm.AtespaceUID, PoolUID: o.pool.UID, Resources: resources, CTID: id, OwnerNonce: store.NewID(), OperationID: store.NewID(), Origin: o.origin})
		if err != nil {
			return err
		}
	} else {
		a, err = o.server.store.GetManagedActor(o.context, o.actor.UID)
		if err != nil {
			return err
		}
		if a.Revision != o.actor.Revision || a.AtespaceUID != o.swarm.AtespaceUID {
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
		// остановлена, и stop, delete и reset его отменяют. Start и update
		// ничего бы не добавили к уже записанному start: отказ называет
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
		if a.Stage != "done" && !waiting && (o.action != "delete" || a.UPID == "" || a.Stage == "delete" || a.Operation == "reset") {
			return fmt.Errorf("%w: previous provider outcome is uncertain; allocation retained", app.ErrConflict)
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
			return fmt.Errorf("%w: owned container is absent; delete the retained allocation before creating another", app.ErrConflict)
		}
		// Update перезапускает работающую CT; остановленную поднимает start.
		if o.action == "update" && observed != "running" {
			return fmt.Errorf("%w: owned container is %s; update restarts a running container", app.ErrConflict, observed)
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
			// update задачи нет вовсе. Слот закрывает наблюдение CT.
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
				noteLifecycleIntent(o.context, lifecycleIntentPossible)
			}
			return err
		})
		release()
		if err != nil {
			if errors.Is(err, store.ErrPoolUnavailable) || errors.Is(err, store.ErrRevisionConflict) || errors.Is(err, app.ErrNotAllowed) {
				return err
			}
			// Отказ координации называет держателя и request ID (#272): общий
			// текст отправил бы сверять задачу провайдера вместо аренды.
			var held *store.CoordinationConflictError
			if errors.As(err, &held) {
				return fmt.Errorf("%w: %w", app.ErrConflict, held)
			}
			return fmt.Errorf("%w: managed task must be reconciled before changing intent", app.ErrConflict)
		}
		noteLifecycleIntent(o.context, lifecycleIntentAccepted)
		if o.action == "stop" || o.action == "delete" || o.action == "reset" || o.action == "update" {
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

func (s *Server) createManagedActor(w http.ResponseWriter, r *http.Request, userID string, sw app.Swarm, input *ateapi.Actor, pool store.ManagedPool, creation actorCreation) {
	// The generated profile is the entire execution definition. Native knobs are
	// not an alternate route for OCI, mounts, egress overrides or arbitrary env.
	if input.GetSourceTag() != nil || input.GetMetadata().GetUid() != "" || input.GetMetadata().GetVersion() != 0 || input.GetMetadata().GetCreateTime() != nil || input.GetMetadata().GetUpdateTime() != nil || input.GetMetadata().GetDeleteTime() != nil {
		writeDetail(w, 400, "managed actors accept only name, approved actorTemplate and workerSelector")
		return
	}
	a, err := s.createManaged(r.Context(), userID, sw, input, pool, creation.role, creation.origin, nil)
	if err != nil {
		managedAPIError(w, r, err)
		return
	}
	writeJSON(w, 201, managedActorJSON(a, pool, sw.AtespaceName, s.settledOriginMode(r.Context(), sw.ProjectID, a.Origin, a.NetworkPolicy)))
}

// createManaged is shared by human creation and UID-bound delegated creation.
// A delegated name retry returns its original reservation without provider writes;
// a retry naming a different origin conflicts. Origin resolution precedes admission.
func (s *Server) createManaged(ctx context.Context, userID string, sw app.Swarm, input *ateapi.Actor, pool store.ManagedPool, role, origin string, parent *store.ManagedActor) (store.ManagedActor, error) {
	if role == "" {
		role = "worker"
	}
	op := &managedWorkloadOperation{server: s, context: ctx, userID: userID, swarm: sw, pool: pool, name: input.GetMetadata().GetName(), action: "create", input: input, role: role, parent: parent}
	s.managed.admission.Lock()
	defer s.managed.admission.Unlock()
	if err := op.authorize(); err != nil {
		return store.ManagedActor{}, err
	}
	if parent != nil {
		a, err := s.store.LookupManagedActor(ctx, sw.ID, op.name)
		if err == nil {
			if a.ParentUID != parent.UID || a.PoolUID != pool.UID || a.Template != parent.Template || a.AtespaceUID != sw.AtespaceUID {
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
	if a.AtespaceUID != sw.AtespaceUID {
		writeDetail(w, 409, "managed actor namespace binding changed")
		return
	}
	if _, err := s.ateapi.GetActor(r.Context(), sw.AtespaceName, a.Name); err == nil {
		writeDetail(w, 409, "native actor conflicts with managed allocation")
		return
	} else {
		var api *substrate.APIError
		if !errors.As(err, &api) || api.Status != 404 {
			apiError(w, r, err)
			return
		}
	}
	p, err := s.store.GetManagedPool(r.Context(), a.PoolUID)
	if err != nil {
		managedAPIError(w, r, err)
		return
	}
	if r.Method == http.MethodGet {
		mode, err := s.originMode(r.Context(), sw.ProjectID, a.Origin, a.NetworkPolicy)
		if err != nil {
			managedAPIError(w, r, err)
			return
		}
		writeJSON(w, 200, managedActorJSON(a, p, sw.AtespaceName, mode))
		return
	}
	if r.Method == http.MethodPut {
		writeDetail(w, 409, "managed profile definition is immutable")
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
			writeDetail(w, 409, "claude-proxmox does not support memory checkpoints; use stop to retain workspace")
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
	writeJSON(w, 200, managedActorJSON(op.saved, p, sw.AtespaceName, s.settledOriginMode(r.Context(), sw.ProjectID, op.saved.Origin, op.saved.NetworkPolicy)))
}
