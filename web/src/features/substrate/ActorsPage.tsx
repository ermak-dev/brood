import { useCallback, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { EmptyState } from '@/components/ui/EmptyState';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';
import { actorsApi, templatesApi, type Actor, type ActorAction } from '@/services/brood/substrate';
import { swarmCapabilities } from '@/features/access/shardState';
import { useAdministrator } from '@/features/access/shared';
import { selectedIn } from '@/features/objects/address';
import { useObjectSelection, usePanel } from '@/features/objects/useObjectSelection';
import { ActorDetailsSheet } from './ActorDetailsSheet';
import { ActorEditor } from './ActorEditor';
import { ActorActionFailure } from './components/ActorActionFailure';
import { ActorRelations } from './components/ActorRelations';
import { ActorOutcomeNotices } from './components/ActorOutcomeNotices';
import { ConfirmDialog } from './components/ConfirmDialog';
import { EntityTable, type Column } from './components/EntityTable';
import { ProjectScope } from './components/ProjectScope';
import { GroupBar, SwarmSections, useSwarmSectionFailure } from './components/SwarmSections';
import { ObjectUnavailable } from './components/SelectionNotice';
import { StateBadge } from './components/StateBadge';
import { DASH, formatDate } from './format';
import {
  actionDialogText,
  actorPlacement,
  actorState,
  canViewActorDetails,
  deleteConfirmation,
  nativeActionGroups,
} from './actorLifecycle';
import { confirmationOf, sessionReadable, type ManagedActionId } from './actorActions';
import { useActorOperations, type UnknownOutcome } from './actorOperations';
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
 * placement. A row opens the actor's side panel (#157), where its actions
 * live: managed actors get their full set and a console, native ones their
 * editor with lifecycle and delete. */
export function ActorsPage() {
  const { t } = useTranslation();
  const admin = useAdministrator();
  return (
    <ProjectScope title={t('actors.title')} addressed>
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

/** Editor of a new actor; `session` remounts it on every open so it starts
 * empty. An existing actor's panel comes from the address instead (#194). */
interface CreateState {
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
  // Отказ загрузки уходит в общую плашку над группами (#255).
  useSwarmSectionFailure(project, name, error);
  const data = loaded?.actors ?? null;
  const goldenOf = (actor: Actor) => loaded?.golden.get(actor.metadata.name);
  const { run, confirm } = useActions(refresh);
  const ops = useActorOperations(project, refresh);
  const [creating, setCreating] = useState<CreateState>({ open: false, session: 0 });
  const [dialog, setDialog] = useState<DialogState | null>(null);

  const openCreate = () => setCreating((prev) => ({ open: true, session: prev.session + 1 }));
  const closeCreate = () => setCreating((prev) => ({ ...prev, open: false }));

  // Панель актора -- по адресу (#194): строка открывает её записью истории,
  // ссылка -- своим адресом, «Назад» закрывает. Актор берётся из свежего
  // списка по имени: после каждого действия панель показывает его нынешнее
  // состояние (после suspend шаблон native-актора становится изменяемым).
  const { selection, open: openPanel, close } = useObjectSelection();
  const selected = selectedIn(selection, project, 'actor');
  const panel = usePanel(selected);
  const closePanel = () => close('actor');
  const open = (actor: Actor) =>
    openPanel({ project: projectId, swarm: project, actor: actor.metadata.name });
  const panelActor = data?.find((a) => a.metadata.name === panel.name);
  // Managed-актор открывается в деталях, native -- в своём редакторе (#157);
  // golden-актора ведёт ateapi, его не открывает и адрес.
  const openable = (a: Actor) => !goldenOf(a) && canViewActorDetails(canManage, a);
  const detailsActor = panelActor?.provider === 'proxmox' ? panelActor : undefined;
  const editorActor =
    panelActor && panelActor.provider !== 'proxmox' && openable(panelActor) ? panelActor : null;
  const selectedActor = selected ? data?.find((a) => a.metadata.name === selected) : undefined;
  const unavailable = Boolean(selected && data && !(selectedActor && openable(selectedActor)));
  // Связи открытого актора (#195): рой и шаблон из того же чтения, что список.
  const relations = (actor: Actor) => (
    <ActorRelations
      actor={actor}
      scope={{ project: projectId, swarm: project }}
      swarmName={name}
      templates={loaded?.templates ?? null}
    />
  );

  // Native-действия: прежний поток, отказ — в той же плашке. Идущее действие
  // помнится по актору: его кнопка в панели ждёт исхода, повтор не уходит.
  const [acting, setActing] = useState<Record<string, ActorAction>>({});
  const mutate = async (actor: Actor, action: ActorAction) => {
    const name = actor.metadata.name;
    setActing((items) => ({ ...items, [name]: action }));
    ops.clearFailure();
    try {
      return await actorsApi.act(project, name, action);
    } catch (error) {
      ops.setFailure({ name, error });
      throw error;
    } finally {
      setActing((items) => {
        const next = { ...items };
        delete next[name];
        return next;
      });
    }
  };
  const act = (actor: Actor, action: ActorAction) => {
    if (acting[actor.metadata.name]) return;
    return run(
      () => mutate(actor, action),
      t(`actors.${action}_done`, { name: actor.metadata.name })
    );
  };

  // Удаление native-актора запускается из его панели; удалённому панель не нужна.
  const remove = (actor: Actor) =>
    confirm(
      deleteConfirmation(actor, t, async () => {
        await actorsApi.remove(project, actor.metadata.name);
        closePanel();
      })
    );

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
    <Button onClick={openCreate}>{t('actors.create')}</Button>
  ) : undefined;

  return (
    <div className={styles.page}>
      <GroupBar
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
      {unavailable && selected && (
        <ObjectUnavailable name={selected} onClose={closePanel} />
      )}

      {loading ? (
        <LoadingSpinner />
      ) : error ? null : data && data.length > 0 ? (
        <ActorTable
          rows={data}
          swarm={project}
          canManage={canManage}
          pending={ops.pending}
          unknown={ops.unknown}
          goldenOf={goldenOf}
          onOpen={open}
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

      {canViewActorDetails(canManage, null) && (
        <ActorEditor
          key={`create-${creating.session}`}
          project={project}
          swarmName={name}
          projectId={projectId}
          actor={null}
          templates={loaded?.templates ?? null}
          open={creating.open}
          onClose={closeCreate}
          onSubmit={async (actor) => {
            if (!canManage) return false;
            const name = actor.metadata.name;
            const ok = await run(() => actorsApi.create(project, actor), t('actors.created', { name }));
            if (ok) closeCreate();
            return ok;
          }}
        />
      )}

      {editorActor && canViewActorDetails(canManage, editorActor) && (
        <ActorEditor
          key={`editor-${panel.session}`}
          project={project}
          swarmName={name}
          projectId={projectId}
          actor={editorActor}
          relations={relations(editorActor)}
          templates={loaded?.templates ?? null}
          open={panel.open}
          onClose={closePanel}
          actions={nativeActionGroups(editorActor, {
            t,
            canManage,
            pending: acting[editorActor.metadata.name],
            terminal: actorsApi.terminal(project, editorActor.metadata.name),
            onAction: (action) => void act(editorActor, action),
            onRemove: () => remove(editorActor),
          }).map((group) => ({ ...group, actions: group.actions.filter((action) => action.id !== 'terminal') }))}
          notice={
            ops.failure?.name === editorActor.metadata.name && (
              <ActorActionFailure
                name={ops.failure.name}
                error={ops.failure.error}
                administrator={administrator}
                onClose={ops.clearFailure}
              />
            )
          }
          onSubmit={async (actor) => {
            if (!canManage) return false;
            const name = actor.metadata.name;
            const ok = await run(() => updateFresh(project, actor), t('actors.saved', { name }));
            if (ok) closePanel();
            return ok;
          }}
        />
      )}

      {detailsActor && canViewActorDetails(canManage, detailsActor) && (
        <ActorDetailsSheet
          key={`details-${panel.session}`}
          open={panel.open}
          onClose={closePanel}
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
          relations={relations(detailsActor)}
        />
      )}

      {dialog && canManage && (
        <ManagedActionDialog
          key={`confirmation-${dialog.session}`}
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
 * cell instead of columns of their own. A row holds no actions (#157): it
 * opens the actor's panel -- managed details for anyone who may read them,
 * the native editor for those who manage the swarm. The state cell still says
 * when an action is in flight or its outcome is unknown, with the panel closed. */
export function ActorTable({
  rows,
  swarm,
  canManage,
  pending = {},
  unknown = [],
  goldenOf,
  onOpen,
}: {
  rows: Actor[];
  swarm: string;
  canManage: boolean;
  pending?: Record<string, ManagedActionId>;
  unknown?: UnknownOutcome[];
  goldenOf: (actor: Actor) => string | undefined;
  onOpen: (actor: Actor) => void;
}) {
  const { t, i18n } = useTranslation();
  const uncertain = new Set(unknown.map((item) => item.name));
  // Golden-актор ведёт ateapi; native-актор viewer'у открыть нечем: редактор
  // меняет его, а сервер этого не пустит.
  const openable = (a: Actor) => !goldenOf(a) && canViewActorDetails(canManage, a);
  const columns: Column<Actor>[] = [
    {
      key: 'name',
      label: t('substrate.name'),
      primary: true,
      render: (a) =>
        goldenOf(a) ? (
          <span title={t('actors.golden_hint', { template: goldenOf(a) })}>
            <span className={`${styles.badge} ${styles['tone-neutral']}`}>
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
          {pending[a.metadata.name] && (
            <span className={styles.pendingNote}>
              <span className="loading-spinner" aria-hidden="true" />
              {t(`actors.ops.${pending[a.metadata.name]}.pending`)}
            </span>
          )}
          {uncertain.has(a.metadata.name) && (
            <span className={`${styles.badge} ${styles['tone-attention']}`}>
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
    <EntityTable
      columns={columns}
      rows={rows}
      rowKey={(a) => a.metadata.uid ?? a.metadata.name}
      onRowClick={rows.some(openable) ? onOpen : undefined}
      canOpen={openable}
      actionsLabel={t('actors.ops.terminal.label')}
      actions={canManage ? (actor) => {
        if (goldenOf(actor)) return null;
        const available = actor.provider !== 'proxmox' || (
          sessionReadable(actor) && actor.capabilities?.terminal && !pending[actor.metadata.name]
        );
        const label = t('actors.ops.terminal.label');
        return available ? (
          <a className={`btn btn-secondary btn-sm ${styles.actorTerminal}`} href={actorsApi.terminal(swarm, actor.metadata.name)} target="_blank" rel="noopener noreferrer">
            {label}<span className={styles.srOnly}> {actor.metadata.name} {t('substrate.opens_new_tab')}</span>
          </a>
        ) : (
          <Button variant="secondary" size="sm" disabled className={styles.actorTerminal} title={t(`actors.ops.reasons.${pending[actor.metadata.name] ? 'pending' : actor.capabilities?.terminal ? 'not_running' : 'no_terminal'}`)}>
            {label}
          </Button>
        );
      } : undefined}
    />
  );
}
