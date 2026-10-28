import { useCallback, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { EmptyState } from '@/components/ui/EmptyState';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';
import { actorsApi, templatesApi, type Actor, type ActorAction } from '@/services/brood/substrate';
import { ActorEditor } from './ActorEditor';
import { EntityTable, type Column } from './components/EntityTable';
import { PageHeader } from './components/PageHeader';
import { ProjectScope } from './components/ProjectScope';
import { SwarmSections } from './components/SwarmSections';
import { StateBadge } from './components/StateBadge';
import { DASH, formatDate, shortState } from './format';
import { useActions, useResource } from './hooks';
import styles from './substrate.module.scss';

const stateOf = (actor: Actor) => shortState(actor.status?.state, 'ACTOR_STATE_');

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
  return (
    <ProjectScope title={t('actors.title')}>
      {(project) => (
        <SwarmSections project={project} title={t('actors.title')}>
          {(swarm) => <Actors project={swarm.id} name={swarm.name} />}
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
  return { actors, templates: templates.map((tpl) => tpl.metadata.name), golden };
}

function Actors({ project, name }: { project: string; name: string }) {
  const { t, i18n } = useTranslation();
  const load = useCallback(() => loadActors(project), [project]);
  const { data: loaded, error, loading, refresh } = useResource(load);
  const data = loaded?.actors ?? null;
  const goldenOf = (actor: Actor) => loaded?.golden.get(actor.metadata.name);
  const { run, confirm } = useActions(refresh);
  const [editor, setEditor] = useState<EditorState>({ actor: null, open: false, session: 0 });

  const openEditor = (actor: Actor | null) =>
    setEditor((prev) => ({ actor, open: true, session: prev.session + 1 }));
  const closeEditor = () => setEditor((prev) => ({ ...prev, open: false }));

  const act = (actor: Actor, action: ActorAction) =>
    run(
      () => actorsApi.act(project, actor.metadata.name, action),
      t(`actors.${action}_done`, { name: actor.metadata.name })
    );

  const remove = (actor: Actor) =>
    confirm({
      title: t('actors.delete_title'),
      message: t('actors.delete_message', { name: actor.metadata.name }),
      confirmText: t('substrate.delete'),
      success: t('actors.deleted', { name: actor.metadata.name }),
      run: () => actorsApi.remove(project, actor.metadata.name),
    });

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
      render: (a) => <StateBadge state={stateOf(a)} namespace="actors.state" />,
    },
    {
      key: 'template',
      label: t('actors.template'),
      mono: true,
      render: (a) => a.actorTemplate?.name || DASH,
    },
    {
      key: 'node',
      label: t('actors.node'),
      hideOnMobile: true,
      render: (a) => a.status?.assignedNode || a.status?.workerAssignment?.nodeName || DASH,
    },
    {
      key: 'updated',
      label: t('substrate.updated'),
      render: (a) => formatDate(a.metadata.updateTime, i18n.language),
    },
  ];

  const count = (state: string) => (data ?? []).filter((a) => stateOf(a) === state).length;
  const create = <Button onClick={() => openEditor(null)}>{t('actors.create')}</Button>;

  return (
    <div className={styles.page}>
      <PageHeader
        title={name}
        headingLevel={2}
        meta={[
          data && t('actors.meta_count', { count: data.length }),
          data && count('running') > 0 && t('actors.meta_running', { count: count('running') }),
          data &&
            count('suspended') > 0 &&
            t('actors.meta_suspended', { count: count('suspended') }),
        ]}
        actions={create}
      />

      {loading ? (
        <LoadingSpinner />
      ) : error ? (
        <div className={styles.empty}>
          <EmptyState title={t('substrate.load_failed')} description={error} />
        </div>
      ) : data && data.length > 0 ? (
        <EntityTable
          columns={columns}
          rows={data}
          rowKey={(a) => a.metadata.uid ?? a.metadata.name}
          onRowClick={(a) => !goldenOf(a) && openEditor(a)}
          actionsLabel={t('substrate.actions')}
          actions={(a) =>
            goldenOf(a) ? null : (
              <>
                <a
                  href={actorsApi.terminal(project, a.metadata.name)}
                  target="_blank"
                  rel="noopener noreferrer"
                >
                  {t('access.terminal')}
                </a>
                {stateOf(a) === 'running' && (
                  <Button variant="ghost" size="sm" onClick={() => void act(a, 'suspend')}>
                    {t('actors.suspend')}
                  </Button>
                )}
                {stateOf(a) === 'suspended' && (
                  <Button variant="ghost" size="sm" onClick={() => void act(a, 'resume')}>
                    {t('actors.resume')}
                  </Button>
                )}
                <Button variant="ghost" size="sm" onClick={() => openEditor(a)}>
                  {t('substrate.edit')}
                </Button>
                <Button
                  variant="ghost"
                  size="sm"
                  className={styles.dangerText}
                  onClick={() => remove(a)}
                >
                  {t('substrate.delete')}
                </Button>
              </>
            )
          }
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

      <ActorEditor
        key={editor.session}
        project={project}
        actor={editor.actor}
        templates={loaded?.templates ?? null}
        open={editor.open}
        onClose={closeEditor}
        onSubmit={async (actor) => {
          const name = actor.metadata.name;
          const ok = await run(
            () => (editor.actor ? updateFresh(project, actor) : actorsApi.create(project, actor)),
            editor.actor ? t('actors.saved', { name }) : t('actors.created', { name })
          );
          if (ok) closeEditor();
          return ok;
        }}
      />
    </div>
  );
}
