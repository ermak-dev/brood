import { useCallback, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { EmptyState } from '@/components/ui/EmptyState';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';
import { actorsApi, templatesApi, type Actor, type ActorAction } from '@/services/brood/substrate';
import { swarmCapabilities } from '@/features/access/shardState';
import { useAdministrator } from '@/features/access/shared';
import { ActorEditor } from './ActorEditor';
import { ActorActionFailure } from './components/ActorActionFailure';
import { EntityTable, type Column } from './components/EntityTable';
import { PageHeader } from './components/PageHeader';
import { ProjectScope } from './components/ProjectScope';
import { SwarmSections } from './components/SwarmSections';
import { StateBadge } from './components/StateBadge';
import { DASH, formatDate } from './format';
import {
  actorPlacement,
  actorState,
  canViewActorDetails,
  deleteConfirmation,
  resetConfirmation,
} from './actorLifecycle';
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
 * placement. */
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
  name,
  canManage,
  administrator,
}: {
  project: string;
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
  const [editor, setEditor] = useState<EditorState>({ actor: null, open: false, session: 0 });

  const openEditor = (actor: Actor | null) =>
    setEditor((prev) => ({ actor, open: true, session: prev.session + 1 }));
  const closeEditor = () => setEditor((prev) => ({ ...prev, open: false }));

  const [actionFailure, setActionFailure] = useState<{ name: string; error: unknown } | null>(null);
  const [acting, setActing] = useState(false);
  const mutate = async (actor: Actor, action: ActorAction) => {
    setActing(true);
    setActionFailure(null);
    try {
      return await actorsApi.act(project, actor.metadata.name, action);
    } catch (error) {
      setActionFailure({ name: actor.metadata.name, error });
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

      {actionFailure && (
        <ActorActionFailure
          name={actionFailure.name}
          error={actionFailure.error}
          administrator={administrator}
          onClose={() => setActionFailure(null)}
        />
      )}

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
          goldenOf={goldenOf}
          onOpen={openEditor}
          onAction={(a, action) =>
            action === 'reset'
              ? confirm(resetConfirmation(a, t, () => mutate(a, 'reset')))
              : void act(a, action)
          }
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
    </div>
  );
}

/** brood: the actors of one swarm. Provider and Proxmox CT share the placement
 * cell instead of columns of their own, and a managed row wraps its five
 * actions between the lifecycle and object groups, so the table fits a 1280px
 * screen with the sidebar open without scrolling. A native row keeps its
 * actions on one line, as before managed actors existed. */
export function ActorTable({
  rows,
  swarm,
  canManage,
  acting,
  goldenOf,
  onOpen,
  onAction,
  onRemove: remove,
}: {
  rows: Actor[];
  swarm: string;
  canManage: boolean;
  acting: boolean;
  goldenOf: (actor: Actor) => string | undefined;
  onOpen: (actor: Actor) => void;
  onAction: (actor: Actor, action: ActorAction) => void;
  onRemove: (actor: Actor) => void;
}) {
  const { t, i18n } = useTranslation();
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
        <StateBadge
          state={stateOf(a)}
          namespace={a.provider === 'proxmox' ? 'actors.managed_state' : 'actors.state'}
        />
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
                          {t(a.provider === 'proxmox' ? 'actors.details' : 'substrate.edit')}
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
                  </div>
                )
            : undefined
        }
      />
    </div>
  );
}
