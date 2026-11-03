import { useCallback, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { EmptyState } from '@/components/ui/EmptyState';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';
import { actorsApi, templatesApi, type Actor, type ActorAction } from '@/services/brood/substrate';
import { swarmCapabilities } from '@/features/access/shardState';
import { useAdministrator } from '@/features/access/shared';
import { ActorDetailsSheet } from './ActorDetailsSheet';
import { ActorEditor } from './ActorEditor';
import { ActorActionFailure } from './components/ActorActionFailure';
import { ActorOutcomeNotices } from './components/ActorOutcomeNotices';
import { ConfirmDialog } from './components/ConfirmDialog';
import { EntityTable, type Column } from './components/EntityTable';
import { ManagedActorActions } from './components/ManagedActorActions';
import { PageHeader } from './components/PageHeader';
import { ProjectScope } from './components/ProjectScope';
import { SwarmSections } from './components/SwarmSections';
import { StateBadge } from './components/StateBadge';
import { DASH, formatDate } from './format';
import {
  actionDialogText,
  actorPlacement,
  actorState,
  canViewActorDetails,
  deleteConfirmation,
} from './actorLifecycle';
import { confirmationOf, type ManagedActionId } from './actorActions';
import { useActorOperations, type UnknownOutcome } from './actorOperations';
import { ActorLifecycleActions } from './components/ActorLifecycleActions';
import { useActions, useResource } from './hooks';
import styles from './substrate.module.scss';

const stateOf = actorState;

/** Update with the preconditions of the actor as it is now: ateapi bumps the
 * version on every write, the controller's state changes included, so the
 * version read when the editor opened is stale after any suspend or resume and
 * every save would fail. The edited fields (template, selector) win. */
async function updateFresh(project: string, edited: Actor) {
  const latest = await actorsApi.get(project, edited.metadata.name);
  return actorsApi.update(project, { ...edited, metadata: latest.metadata });
}

/** brood: actors of the active project -- the units of work. Suspend freezes
 * an actor into a snapshot, resume wakes it; editing changes its template and
 * placement. Managed actors get their full set of actions and a console. */
export function ActorsPage() {
  const { t } = useTranslation();
  const admin = useAdministrator();
  return (
    <ProjectScope title={t('actors.title')}>
      {(project) => (
        <SwarmSections project={project} title={t('actors.title')}>
          {(swarm) => (
            <Actors
              key={swarm.id}
              project={swarm.id}
              projectId={project.id}
              name={swarm.name}
              administrator={admin}
              canManage={swarmCapabilities(swarm, admin).manage}
            />
          )}
        </SwarmSections>
      )}
    </ProjectScope>
  );
}

/** Which actor the editor is open for (null -- a new one); `session` remounts
 * the editor on every open so it starts from the object as read. */
interface EditorState {
  actor: Actor | null;
  open: boolean;
  session: number;
}

/** Managed-актор в деталях — по имени: строка списка обновляется после
 * каждого действия, и детали показывают её текущее состояние. */
interface DetailsState {
  name: string;
  open: boolean;
  session: number;
}

/** Подтверждение managed-действия; остаётся смонтированным закрытым, чтобы
 * фокус вернулся к кнопке, которая его открыла. */
interface DialogState {
  actor: Actor;
  id: 'restart' | 'recycle' | 'wipe' | 'reset' | 'delete';
  open: boolean;
  session: number;
}

/** Actors and templates in one load: the editor picks a template from them,
 * and a template's golden actor -- named by the template's uid, created by
 * ateapi to take the golden snapshot -- is told apart from the user's own. */
async function loadActors(project: string) {
  const [actors, templates] = await Promise.all([
    actorsApi.list(project),
    templatesApi.list(project),
  ]);
  const golden = new Map(templates.map((tpl) => [tpl.metadata.uid ?? '', tpl.metadata.name]));
  return { actors, templates, golden };
}

function Actors({
  project,
  projectId,
  name,
  canManage,
  administrator,
}: {
  project: string;
  projectId: string;
  name: string;
  canManage: boolean;
  administrator: boolean;
}) {
  const { t } = useTranslation();
  const load = useCallback(() => loadActors(project), [project]);
  const { data: loaded, error, loading, refresh } = useResource(load);
  const data = loaded?.actors ?? null;
  const goldenOf = (actor: Actor) => loaded?.golden.get(actor.metadata.name);
  const { run, confirm } = useActions(refresh);
  const ops = useActorOperations(project, refresh);
  const [editor, setEditor] = useState<EditorState>({ actor: null, open: false, session: 0 });
  const [details, setDetails] = useState<DetailsState>({ name: '', open: false, session: 0 });
  const [dialog, setDialog] = useState<DialogState | null>(null);

  const openEditor = (actor: Actor | null) =>
    setEditor((prev) => ({ actor, open: true, session: prev.session + 1 }));
  const closeEditor = () => setEditor((prev) => ({ ...prev, open: false }));
  const open = (actor: Actor) =>
    actor.provider === 'proxmox'
      ? setDetails((prev) => ({ name: actor.metadata.name, open: true, session: prev.session + 1 }))
      : openEditor(actor);
  const detailsActor = data?.find((a) => a.metadata.name === details.name);

  // Native-действия: прежний поток с общим флагом, отказ — в той же плашке.
  const [acting, setActing] = useState(false);
  const mutate = async (actor: Actor, action: ActorAction) => {
    setActing(true);
    ops.clearFailure();
    try {
      return await actorsApi.act(project, actor.metadata.name, action);
    } catch (error) {
      ops.setFailure({ name: actor.metadata.name, error });
      throw error;
    } finally {
      setActing(false);
    }
  };
  const act = (actor: Actor, action: ActorAction) => {
    if (acting) return;
    return run(
      () => mutate(actor, action),
      t(`actors.${action}_done`, { name: actor.metadata.name })
    );
  };

  const remove = (actor: Actor) =>
    confirm(deleteConfirmation(actor, t, () => actorsApi.remove(project, actor.metadata.name)));

  // Managed: обратимое — сразу, остальное — через подтверждение с последствиями.
  const request = (actor: Actor, id: ManagedActionId) => {
    if (id === 'terminal') return;
    if (confirmationOf(id) === 'none') {
      void ops.perform(actor, id);
      return;
    }
    setDialog((prev) => ({
      actor,
      id: id as DialogState['id'],
      open: true,
      session: (prev?.session ?? 0) + 1,
    }));
  };
  const closeDialog = () => setDialog((prev) => prev && { ...prev, open: false });

  const count = (state: string) => (data ?? []).filter((a) => stateOf(a) === state).length;
  const create = canManage ? (
    <Button onClick={() => openEditor(null)}>{t('actors.create')}</Button>
  ) : undefined;

  return (
    <div className={styles.page}>
      <PageHeader
        title={name}
        headingLevel={2}
        meta={[
          data && t('actors.meta_count', { count: data.length }),
          data && count('running') > 0 && t('actors.meta_running', { count: count('running') }),
          data && count('stopped') > 0 && t('actors.meta_stopped', { count: count('stopped') }),
          data &&
            count('suspended') > 0 &&
            t('actors.meta_suspended', { count: count('suspended') }),
        ]}
        actions={create}
      />

      {ops.failure && (
        <ActorActionFailure
          name={ops.failure.name}
          action={ops.failure.action ? t(`actors.ops.${ops.failure.action}.label`) : undefined}
          error={ops.failure.error}
          administrator={administrator}
          onClose={ops.clearFailure}
        />
      )}
      <ActorOutcomeNotices items={ops.unknown} onDismiss={ops.dismissUnknown} />

      {loading ? (
        <LoadingSpinner />
      ) : error ? (
        <div className={styles.empty}>
          <EmptyState title={t('substrate.load_failed')} description={error} />
        </div>
      ) : data && data.length > 0 ? (
        <ActorTable
          rows={data}
          swarm={project}
          canManage={canManage}
          acting={acting}
          pending={ops.pending}
          unknown={ops.unknown}
          goldenOf={goldenOf}
          onOpen={open}
          onAction={(a, action) => void act(a, action)}
          onManaged={request}
          onRemove={remove}
        />
      ) : (
        <div className={styles.empty}>
          <EmptyState
            title={t('actors.empty')}
            description={t('actors.empty_hint')}
            action={create}
          />
        </div>
      )}

      {canViewActorDetails(canManage, editor.actor) && (
        <ActorEditor
          key={editor.session}
          project={project}
          projectId={projectId}
          actor={editor.actor}
          templates={loaded?.templates ?? null}
          open={editor.open}
          onClose={closeEditor}
          onSubmit={async (actor) => {
            if (!canManage) return false;
            const name = actor.metadata.name;
            const ok = await run(
              () => (editor.actor ? updateFresh(project, actor) : actorsApi.create(project, actor)),
              editor.actor ? t('actors.saved', { name }) : t('actors.created', { name })
            );
            if (ok) closeEditor();
            return ok;
          }}
        />
      )}

      {detailsActor && canViewActorDetails(canManage, detailsActor) && (
        <ActorDetailsSheet
          key={details.session}
          open={details.open}
          onClose={() => setDetails((prev) => ({ ...prev, open: false }))}
          actor={detailsActor}
          swarm={project}
          swarmName={name}
          canManage={canManage}
          administrator={administrator}
          pending={ops.pending[detailsActor.metadata.name]}
          failure={ops.failure}
          unknown={ops.unknown}
          onAction={(id) => request(detailsActor, id)}
          onClearFailure={ops.clearFailure}
          onDismissUnknown={ops.dismissUnknown}
        />
      )}

      {dialog && canManage && (
        <ManagedActionDialog
          key={dialog.session}
          dialog={dialog}
          unknown={ops.unknown}
          onCancel={closeDialog}
          onConfirm={() => {
            closeDialog();
            void ops.perform(dialog.actor, dialog.id);
          }}
        />
      )}
    </div>
  );
}

/** Подтверждение называет последствие действия; необратимое — ввод имени.
 * Если прежний исход того же действия неизвестен, это сказано до повтора. */
function ManagedActionDialog({
  dialog,
  unknown,
  onCancel,
  onConfirm,
}: {
  dialog: DialogState;
  unknown: UnknownOutcome[];
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const { t } = useTranslation();
  const { actor, id } = dialog;
  const name = actor.metadata.name;
  const text = actionDialogText(actor, id, t);
  const previous = unknown.some((item) => item.name === name && item.action === id);
  return (
    <ConfirmDialog
      open={dialog.open}
      title={text.title}
      confirmText={text.confirmText}
      typedName={confirmationOf(id) === 'typed' ? name : undefined}
      onCancel={onCancel}
      onConfirm={onConfirm}
    >
      <p>{text.message}</p>
      {text.note && <p className={styles.muted}>{text.note}</p>}
      {previous && (
        <p role="alert" className={`${styles.notice} ${styles.unknownNotice}`}>
          {t('actors.ops.previous_unknown', { action: t(`actors.ops.${id}.label`) })}
        </p>
      )}
    </ConfirmDialog>
  );
}

/** brood: the actors of one swarm. Provider and Proxmox CT share the placement
 * cell instead of columns of their own. A managed row shows its state-driven
 * primary action and Stop, then details and a grouped menu of the rest; a
 * native row keeps its actions on one line, as before managed actors existed. */
export function ActorTable({
  rows,
  swarm,
  canManage,
  acting,
  pending = {},
  unknown = [],
  goldenOf,
  onOpen,
  onAction,
  onManaged = () => {},
  onRemove: remove,
}: {
  rows: Actor[];
  swarm: string;
  canManage: boolean;
  acting: boolean;
  pending?: Record<string, ManagedActionId>;
  unknown?: UnknownOutcome[];
  goldenOf: (actor: Actor) => string | undefined;
  onOpen: (actor: Actor) => void;
  onAction: (actor: Actor, action: ActorAction) => void;
  onManaged?: (actor: Actor, id: ManagedActionId) => void;
  onRemove: (actor: Actor) => void;
}) {
  const { t, i18n } = useTranslation();
  const uncertain = new Set(unknown.map((item) => item.name));
  const columns: Column<Actor>[] = [
    {
      key: 'name',
      label: t('substrate.name'),
      primary: true,
      render: (a) =>
        goldenOf(a) ? (
          <span title={t('actors.golden_hint', { template: goldenOf(a) })}>
            <span className={`${styles.badge} ${styles['tone-muted']}`}>
              {t('actors.golden', { template: goldenOf(a) })}
            </span>
          </span>
        ) : (
          a.metadata.name
        ),
    },
    {
      key: 'state',
      label: t('substrate.state'),
      render: (a) => (
        <span className={styles.stateCell}>
          <StateBadge
            state={stateOf(a)}
            namespace={a.provider === 'proxmox' ? 'actors.managed_state' : 'actors.state'}
          />
          {uncertain.has(a.metadata.name) && (
            <span className={`${styles.badge} ${styles['tone-warning']}`}>
              {t('actors.ops.unconfirmed_badge')}
            </span>
          )}
        </span>
      ),
    },
    {
      key: 'template',
      label: t('actors.template'),
      mono: true,
      render: (a) => a.actorTemplate?.name || DASH,
    },
    {
      key: 'placement',
      label: t('substrate.placement'),
      render: actorPlacement,
    },
    {
      key: 'updated',
      label: t('substrate.updated'),
      render: (a) => formatDate(a.metadata.updateTime, i18n.language),
    },
  ];

  return (
    <div className={styles.actorTable}>
      <EntityTable
        columns={columns}
        rows={rows}
        rowKey={(a) => a.metadata.uid ?? a.metadata.name}
        onRowClick={canManage ? (a) => !goldenOf(a) && onOpen(a) : undefined}
        actionsLabel={t('substrate.actions')}
        actions={
          canManage || rows.some((a) => a.provider === 'proxmox')
            ? (a) =>
                goldenOf(a) ? null : (
                  <div
                    className={
                      a.provider === 'proxmox' ? styles.actorActionsWrap : styles.actorActions
                    }
                  >
                    {a.provider === 'proxmox' ? (
                      <ManagedActorActions
                        actor={a}
                        swarm={swarm}
                        canManage={canManage}
                        pending={pending[a.metadata.name]}
                        onAction={(id) => onManaged(a, id)}
                        onDetails={() => onOpen(a)}
                      />
                    ) : (
                      <>
                        <span className={styles.actorActionGroup}>
                          {canManage && (
                            <ActorLifecycleActions
                              actor={a}
                              swarm={swarm}
                              disabled={acting}
                              onAction={(action) => onAction(a, action)}
                            />
                          )}
                        </span>
                        <span className={styles.actorActionGroup}>
                          {canViewActorDetails(canManage, a) && (
                            <Button variant="ghost" size="sm" onClick={() => onOpen(a)}>
                              {t('substrate.edit')}
                            </Button>
                          )}
                          {canManage && (
                            <Button
                              variant="ghost"
                              size="sm"
                              className={styles.dangerText}
                              onClick={() => remove(a)}
                            >
                              {t('substrate.delete')}
                            </Button>
                          )}
                        </span>
                      </>
                    )}
                  </div>
                )
            : undefined
        }
      />
    </div>
  );
}
