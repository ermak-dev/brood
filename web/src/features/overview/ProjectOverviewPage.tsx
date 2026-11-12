/**
 * Обзор проекта (#197) -- стартовая страница панели: что в выбранном проекте
 * сейчас и что ждёт оператора. Рои и их исполнение -- из строки списка роёв
 * (#234, #266), без запроса на рой, как и «Последние прогоны» (момент
 * последнего исполнения, #268); ёмкость, репозиторий, участники и каталог
 * определений -- теми же маршрутами, что страница проекта, по одному запросу.
 * Один опрос на страницу (#257) перечитывает только список роёв. Управление
 * (ёмкость, репозиторий, состав, графы определений) -- на странице проекта,
 * работа роя -- на его странице: обзор ведёт туда ссылками, определение --
 * к себе в каталоге (#270).
 */
import { useCallback, useEffect, useId, type ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { EmptyState } from '@/components/ui/EmptyState';
import {
  accessApi,
  type AccessProject,
  type AccessSwarm,
  type ShardCapacity,
} from '@/services/brood/access';
import { pipelinesApi, type SnapshotIdentity } from '@/services/brood/pipelines';
import { ExecutionMoment } from '@/features/access/ExecutionMoment';
import { LoadState, StatusBadge, useAdministrator } from '@/features/access/shared';
import { swarmTone, type BadgeTone } from '@/features/access/state';
import { executionActivityTone, swarmActivityOf } from '@/features/access/swarmActivity';
import { swarmFactsOf } from '@/features/access/swarmFacts';
import { useReadMoment } from '@/features/access/useReadMoment';
import { ObjectLink } from '@/features/objects/ObjectLink';
import { PageHeader } from '@/features/substrate/components/PageHeader';
import { ProjectScope } from '@/features/substrate/components/ProjectScope';
import { useResource } from '@/features/substrate/hooks';
import { DASH, formatCpu, formatMemory } from '@/features/substrate/format';
import { SWARMS_PATH } from '@/features/swarms/routes';
import { useSwarmPolling } from '@/features/swarms/useSwarmPolling';
import { useProjectStore, useSwarmActivityStore } from '@/stores';
import { formatDateTimeValue } from '@/utils/format';
import {
  overviewSummary,
  overviewSwarms,
  projectDetailsPath,
  recentRuns,
  summaryTarget,
} from './overview';
import styles from '@/features/substrate/substrate.module.scss';
import local from './ProjectOverview.module.scss';

/** Сколько участников обзор называет по имени; остальные -- числом. */
const NAMED_MEMBERS = 8;

export function ProjectOverviewPage() {
  const { t } = useTranslation();
  return (
    <ProjectScope
      title={t('project_overview.title')}
      addressed
      empty={(projects) => <ProjectChoice projects={projects} />}
    >
      {(project) => <ProjectOverview project={project} />}
    </ProjectScope>
  );
}

/** Без выбранного проекта: выбор из доступных прямо здесь -- обзор остаётся
 * стартовой страницей; без проектов -- как его получить. Отозванный проект
 * другим не подменяется (ProjectScope). */
function ProjectChoice({ projects }: { projects: AccessProject[] }) {
  const { t } = useTranslation();
  const titleId = useId();
  const administrator = useAdministrator();
  const setProject = useProjectStore((state) => state.setProject);
  if (projects.length === 0) {
    return (
      <div className={styles.empty}>
        <EmptyState
          title={t('project_overview.none_title')}
          description={t(
            administrator ? 'project_overview.none_admin' : 'project_overview.none_member'
          )}
          action={
            administrator ? (
              <Link className="btn btn-primary" to="/projects">
                {t('project_overview.open_projects')}
              </Link>
            ) : undefined
          }
        />
      </div>
    );
  }
  return (
    <section className={local.card} aria-labelledby={titleId} data-overview="choice">
      <h2 id={titleId} className={local.cardTitle}>
        {t('project_overview.choose_title')}
      </h2>
      <p className={local.hint}>{t('project_overview.choose_hint')}</p>
      <ul className={local.choices}>
        {projects.map((project) => (
          <li key={project.id}>
            <button
              type="button"
              className={local.choice}
              data-choose-project={project.id}
              onClick={() => setProject(project.id)}
            >
              <span className={local.choiceName}>{project.name}</span>
              {Number.isSafeInteger(project.swarmCount) && (
                <span className={local.choiceMeta}>
                  {t('project_overview.project_swarms', { count: project.swarmCount })}
                </span>
              )}
            </button>
          </li>
        ))}
      </ul>
    </section>
  );
}

function ProjectOverview({ project }: { project: AccessProject }) {
  const { t } = useTranslation();
  const id = project.id;
  const loadSwarms = useCallback(() => accessApi.swarms(id), [id]);
  const swarms = useResource(loadSwarms);
  // Один тик на страницу (#257): перечитывается только список роёв, его
  // строка несёт исполнение, вопросы и недоставленные ответы каждого роя.
  useSwarmPolling(swarms.refresh, true);
  // Меню считает ждущих по общему списку (#255): обзор отдаёт ему свой.
  const adopt = useSwarmActivityStore((state) => state.adopt);
  useEffect(() => {
    if (swarms.data) adopt(id, swarms.data);
  }, [adopt, id, swarms.data]);
  const details = projectDetailsPath(id);
  return (
    <div className={`${styles.page} ${local.page}`}>
      <PageHeader
        title={project.name}
        meta={[t('project_overview.title')]}
        actions={
          <Link className="btn btn-secondary" to={details}>
            {t('project_overview.details')}
          </Link>
        }
      />
      {swarms.data && <Summary rows={swarms.data} />}
      <div className={local.layout}>
        <div className={local.column}>
          <Swarms
            rows={swarms.data}
            loading={swarms.loading}
            error={swarms.error}
            retry={swarms.refresh}
          />
          {swarms.data && <RecentRuns rows={swarms.data} />}
          <Workflows projectId={id} details={details} />
        </div>
        <div className={local.column}>
          <Repository projectId={id} />
          <Capacity projectId={id} />
          <Members projectId={id} />
        </div>
      </div>
    </div>
  );
}

/** Блок обзора: заголовок, необязательная ссылка справа и пояснение. */
function Card({
  name,
  title,
  link,
  hint,
  children,
}: {
  name: string;
  title: string;
  link?: ReactNode;
  hint?: string;
  children: ReactNode;
}) {
  const titleId = useId();
  return (
    <section className={local.card} aria-labelledby={titleId} data-overview={name}>
      <header className={local.cardHead}>
        <h2 id={titleId} className={local.cardTitle}>
          {title}
        </h2>
        {link}
      </header>
      {hint && <p className={local.hint}>{hint}</p>}
      {children}
    </section>
  );
}

/** Ссылка блока на раздел, а не на объект: список роёв, каталог. */
function CardLink({ to, children }: { to: string; children: ReactNode }) {
  return (
    <Link to={to} className={local.cardLink}>
      {children}
      <span aria-hidden="true"> →</span>
    </Link>
  );
}

interface TileSpec {
  name: string;
  label: string;
  value: string;
  hint?: string;
  tone: BadgeTone;
  rows: AccessSwarm[] | null;
}

/** Плитки «что ждёт оператора»: число роёв (у недоставленных -- ответов),
 * тон -- когда смотреть есть на что. Плитка ведёт к рою, где действовать
 * (summaryTarget). */
function Summary({ rows }: { rows: AccessSwarm[] }) {
  const { t } = useTranslation();
  const summary = overviewSummary(rows);
  const count = (list: AccessSwarm[]) => String(list.length);
  const tiles: TileSpec[] = [
    {
      name: 'waiting',
      label: t('project_overview.waiting'),
      value: count(summary.waiting),
      hint: t('project_overview.open_requests', { count: summary.openRequests }),
      tone: 'attention',
      rows: summary.waiting,
    },
    {
      name: 'undelivered',
      label: t('project_overview.undelivered'),
      value: summary.undeliveredAnswers === null ? DASH : String(summary.undeliveredAnswers),
      hint: t(
        summary.undelivered === null
          ? 'project_overview.undelivered_unknown'
          : 'project_overview.undelivered_hint'
      ),
      tone: 'attention',
      rows: summary.undelivered,
    },
    {
      name: 'running',
      label: t('project_overview.running'),
      value: count(summary.running),
      hint: t('project_overview.running_hint'),
      tone: 'progress',
      rows: summary.running,
    },
    {
      name: 'stuck',
      label: t('project_overview.stuck'),
      value: count(summary.stuck),
      hint: t('project_overview.stuck_hint'),
      tone: 'problem',
      rows: summary.stuck,
    },
  ];
  return (
    <section
      className={local.summary}
      aria-label={t('project_overview.summary_label')}
      data-overview="summary"
    >
      <div className={local.tiles}>
        {tiles.map((tile) => (
          <Tile key={tile.name} tile={tile} />
        ))}
      </div>
      {summary.unknown > 0 && (
        <p className={local.hint}>
          {t('project_overview.unknown_swarms', { count: summary.unknown })}
        </p>
      )}
    </section>
  );
}

function Tile({ tile }: { tile: TileSpec }) {
  const target = tile.rows ? summaryTarget(tile.rows) : undefined;
  const body = (
    <>
      <span className={local.tileLabel}>{tile.label}</span>
      <strong className={local.tileValue}>{tile.value}</strong>
      {tile.hint && <span className={local.tileHint}>{tile.hint}</span>}
    </>
  );
  return (
    <div
      className={local.tile}
      data-tile={tile.name}
      data-tone={target ? tile.tone : 'neutral'}
    >
      {target ? (
        <Link to={target} className={local.tileLink}>
          {body}
        </Link>
      ) : (
        <div className={local.tileBody}>{body}</div>
      )}
    </div>
  );
}

function Swarms({
  rows,
  loading,
  error,
  retry,
}: {
  rows: AccessSwarm[] | null;
  loading: boolean;
  error: string | null;
  retry: () => Promise<void>;
}) {
  const { t } = useTranslation();
  const listed = rows ? overviewSwarms(rows) : null;
  const link =
    rows && rows.length > 0 ? (
      <CardLink to={SWARMS_PATH}>{t('access.all_swarms')}</CardLink>
    ) : undefined;
  return (
    <Card
      name="swarms"
      title={t('access.swarms')}
      link={link}
      hint={t('project_overview.swarms_hint')}
    >
      {!listed ? (
        <LoadState loading={loading} error={error ?? ''} retry={retry}>
          {null}
        </LoadState>
      ) : listed.shown.length === 0 ? (
        <div className={styles.empty}>
          <EmptyState
            title={t('project_overview.no_swarms')}
            description={t('project_overview.no_swarms_hint')}
            action={
              <Link className="btn btn-primary" to={SWARMS_PATH}>
                {t('project_overview.open_swarms')}
              </Link>
            }
          />
        </div>
      ) : (
        <>
          <ul className={local.swarms}>
            {listed.shown.map((row) => (
              <SwarmRow key={row.id} swarm={row} />
            ))}
          </ul>
          {listed.hidden > 0 && (
            <p className={local.hint}>
              {t('project_overview.hidden_swarms', { count: listed.hidden })}
            </p>
          )}
        </>
      )}
      {/* Список уже показан, а очередное чтение не удалось: прежние строки
          остаются, причина -- рядом, следующий тик повторит. */}
      {listed && error && (
        <p role="status" className={local.hint}>
          {t('project_overview.refresh_failed', { reason: error })}
        </p>
      )}
    </Card>
  );
}

/** Рой в обзоре: имя ссылкой на его страницу (прогон -- это рой, #206),
 * исполнение, открытые вопросы и недоставленные ответы; под ними сведения
 * строки (#268); у активного роя -- ссылки на его акторов и шаблоны.
 * Состояние инфраструктуры -- только когда рой не active: здоровое
 * состояние покоя не красится (#254). */
function SwarmRow({ swarm }: { swarm: AccessSwarm }) {
  const { t } = useTranslation();
  const activity = swarmActivityOf(swarm);
  const undelivered = activity?.undeliveredAnswers ?? 0;
  const active = swarm.state === 'active';
  const scope = { project: swarm.project_id, swarm: swarm.id };
  return (
    <li className={local.swarm} data-swarm={swarm.id}>
      <div className={local.swarmName}>
        <ObjectLink object={{ kind: 'swarm', id: swarm.id }}>{swarm.name}</ObjectLink>
        {!active && (
          <StatusBadge tone={swarmTone(swarm.state)}>
            {t(`access.states.${swarm.state}`, { defaultValue: swarm.state })}
          </StatusBadge>
        )}
      </div>
      <div className={local.swarmNow}>
        {activity ? (
          <StatusBadge tone={executionActivityTone(activity.execution)}>
            {t(`access.execution_states.${activity.execution}`, {
              defaultValue: activity.execution,
            })}
          </StatusBadge>
        ) : (
          <span className={styles.muted} title={t('project_overview.no_summary')}>
            {DASH}
          </span>
        )}
        {activity && activity.openRequests > 0 && (
          <StatusBadge tone="attention">
            {t('project_overview.open_requests', { count: activity.openRequests })}
          </StatusBadge>
        )}
        {undelivered > 0 && (
          <StatusBadge tone="attention">
            {t('project_overview.undelivered_answers', { count: undelivered })}
          </StatusBadge>
        )}
      </div>
      <SwarmFacts swarm={swarm} />
      {active && (
        <ul className={local.swarmObjects} aria-label={t('project_overview.swarm_objects')}>
          <li>
            <ObjectLink object={{ kind: 'actor', ...scope }}>{t('nav.actors')}</ObjectLink>
          </li>
          <li>
            <ObjectLink object={{ kind: 'template', ...scope }}>{t('nav.templates')}</ObjectLink>
          </li>
        </ul>
      )}
    </li>
  );
}

/** Сведения строки роя (#268): определение снимка -- имя ссылкой на него в
 * каталоге проекта (#270) и версия снимка, число участников и managed-акторов.
 * Managed-акторы так и названы: native-акторов строка не считает, и «Акторы»
 * рядом ведут ко всем. Строка старого сервера сведений не несёт -- и строки
 * под исполнением нет, как прежде. */
function SwarmFacts({ swarm }: { swarm: AccessSwarm }) {
  const { t } = useTranslation();
  const { definition, members, managedActors } = swarmFactsOf(swarm);
  if (!definition && members === undefined && managedActors === undefined) return null;
  return (
    <p className={local.swarmFacts} data-swarm-facts="">
      {definition && <SnapshotDefinition project={swarm.project_id} definition={definition} />}
      {members !== undefined && (
        <span>{t('project_overview.swarm_members', { count: members })}</span>
      )}
      {managedActors !== undefined && (
        <span title={t('access.managed_actors_hint')}>
          {t('project_overview.managed_actors', { count: managedActors })}
        </span>
      )}
    </p>
  );
}

/** Определение снимка роя: имя ссылкой на него в каталоге проекта (#270) и
 * версия снимка -- не текущая версия каталога, о чём говорит подсказка. */
function SnapshotDefinition({
  project,
  definition,
}: {
  project: string;
  definition: SnapshotIdentity;
}) {
  const { t } = useTranslation();
  return (
    <span>
      {t('access.workflow')}{' '}
      <ObjectLink object={{ kind: 'workflow', project, name: definition.name }} />{' '}
      <span title={t('swarm_page.snapshot.hint')}>
        {t('swarm_page.snapshot.version_short', { version: definition.version })}
      </span>
    </span>
  );
}

/** «Последние прогоны» (#268): рои по моменту последнего исполнения из их
 * строк (recentRuns), без запроса на рой; сколько прошло -- от чтения списка,
 * опрос страницы его обновляет. Ни один рой не запускался -- так и сказано;
 * моментов не несёт ни одна строка, хотя рои шли (старый сервер), -- блока
 * нет: сказать о прогонах ему нечего. */
function RecentRuns({ rows }: { rows: AccessSwarm[] }) {
  const { t } = useTranslation();
  const now = useReadMoment(rows);
  const runs = recentRuns(rows);
  if (runs === null) return null;
  return (
    <Card
      name="runs"
      title={t('project_overview.recent_runs')}
      hint={t('project_overview.recent_runs_hint')}
    >
      {runs.length === 0 ? (
        <p className={local.hint}>{t('project_overview.no_runs')}</p>
      ) : (
        <ul className={local.swarms}>
          {runs.map((row) => (
            <RunRow key={row.id} swarm={row} now={now} />
          ))}
        </ul>
      )}
    </Card>
  );
}

/** Прогон -- это рой (#206): имя ссылкой на его страницу, исполнение и
 * сколько прошло с его смены; под ними -- определение снимка, если оно
 * записано. */
function RunRow({ swarm, now }: { swarm: AccessSwarm; now: number }) {
  const { t } = useTranslation();
  const activity = swarmActivityOf(swarm);
  const definition = swarmFactsOf(swarm).definition;
  if (!activity || activity.executionAt === undefined) return null;
  return (
    <li className={local.swarm} data-run={swarm.id}>
      <div className={local.swarmName}>
        <ObjectLink object={{ kind: 'swarm', id: swarm.id }}>{swarm.name}</ObjectLink>
      </div>
      <div className={local.swarmNow}>
        <StatusBadge tone={executionActivityTone(activity.execution)}>
          {t(`access.execution_states.${activity.execution}`, {
            defaultValue: activity.execution,
          })}
        </StatusBadge>
        <ExecutionMoment at={activity.executionAt} now={now} />
      </div>
      {definition && (
        <p className={local.swarmFacts}>
          <SnapshotDefinition project={swarm.project_id} definition={definition} />
        </p>
      )}
    </li>
  );
}

/** Каталог определений проекта (#240): имя, версия, момент публикации.
 * Графы -- на странице проекта: имя ведёт к своему определению в каталоге
 * (#270), ссылка блока -- к каталогу. */
function Workflows({ projectId, details }: { projectId: string; details: string }) {
  const { t, i18n } = useTranslation();
  const load = useCallback(() => pipelinesApi.listDefinitions(projectId), [projectId]);
  const catalog = useResource(load);
  const definitions = catalog.data;
  return (
    <Card
      name="workflows"
      title={t('project_overview.workflows')}
      link={<CardLink to={details}>{t('project_overview.catalog_link')}</CardLink>}
      hint={t('project_overview.workflows_hint')}
    >
      <LoadState loading={catalog.loading} error={catalog.error ?? ''} retry={catalog.refresh}>
        {definitions && definitions.length > 0 ? (
          <ul className={local.definitions}>
            {definitions.map((definition) => {
              const time = formatDateTimeValue(definition.publishedAt, i18n.language);
              return (
                <li key={definition.name} className={local.definition}>
                  <span className={local.definitionName}>
                    <ObjectLink
                      object={{ kind: 'workflow', project: projectId, name: definition.name }}
                    />
                  </span>
                  <span className={local.definitionMeta}>
                    {t('pipelines.catalog.version', { version: definition.version })}
                    {' · '}
                    {time
                      ? t('pipelines.catalog.published', { time })
                      : t('pipelines.catalog.published_unknown')}
                  </span>
                </li>
              );
            })}
          </ul>
        ) : (
          <p className={local.hint}>{t('pipelines.catalog.empty_hint')}</p>
        )}
      </LoadState>
    </Card>
  );
}

function Repository({ projectId }: { projectId: string }) {
  const { t } = useTranslation();
  const load = useCallback(() => accessApi.repository(projectId), [projectId]);
  const repository = useResource(load);
  return (
    <Card name="repository" title={t('access.repository.title')}>
      <LoadState
        loading={repository.loading}
        error={repository.error ?? ''}
        retry={repository.refresh}
      >
        {repository.data ? (
          <code className={local.origin}>{repository.data}</code>
        ) : (
          <p className={local.hint}>{t('access.repository.not_set')}</p>
        )}
      </LoadState>
    </Card>
  );
}

type Limit = keyof Omit<ShardCapacity, 'worker_selector'>;

/** Резерв и пределы проекта (#48): «занято из предела». Неизвестный резерв
 * -- причиной, не нулём; пустой предел -- без ограничения. */
function Capacity({ projectId }: { projectId: string }) {
  const { t } = useTranslation();
  const load = useCallback(() => accessApi.project(projectId), [projectId]);
  const project = useResource(load);
  const detail = project.data;
  const rows: Array<{
    key: string;
    label: string;
    used: string | undefined;
    limit: Limit;
    format: (value: string) => string;
  }> = [
    {
      key: 'swarms',
      label: t('access.swarms'),
      used: detail?.usage?.swarms,
      limit: 'max_swarms',
      format: (value) => value,
    },
    {
      key: 'actors',
      label: t('actors.title'),
      used: detail?.usage?.actors,
      limit: 'max_actors',
      format: (value) => value,
    },
    {
      key: 'cpu',
      label: t('project_overview.cpu'),
      used: detail?.usage?.cpu_millicores,
      limit: 'max_cpu_millicores',
      format: formatCpu,
    },
    {
      key: 'memory',
      label: t('project_overview.memory'),
      used: detail?.usage?.memory_bytes,
      limit: 'max_memory_bytes',
      format: formatMemory,
    },
  ];
  const known = Boolean(detail?.usage_available && detail.usage);
  const value = (row: (typeof rows)[number]) => {
    const limit = detail?.capacity[row.limit] ?? null;
    if (!known || row.used === undefined) {
      return limit === null
        ? t('access.unlimited')
        : t('project_overview.limit_only', { limit: row.format(limit) });
    }
    const used = row.format(row.used);
    return limit === null
      ? t('project_overview.used_unlimited', { used })
      : t('project_overview.used_of', { used, limit: row.format(limit) });
  };
  return (
    <Card name="capacity" title={t('project_overview.capacity')}>
      <LoadState loading={project.loading} error={project.error ?? ''} retry={project.refresh}>
        {detail && (
          <>
            {!known && (
              <p role="status" className={local.hint}>
                {t('access.usage_unknown')}
                {detail.usage_error ? `: ${detail.usage_error}` : ''}
              </p>
            )}
            <dl className={local.facts}>
              {rows.map((row) => (
                <div key={row.key}>
                  <dt>{row.label}</dt>
                  <dd>{value(row)}</dd>
                </div>
              ))}
            </dl>
          </>
        )}
      </LoadState>
    </Card>
  );
}

/** Участники проекта: имена первых NAMED_MEMBERS, остальные -- числом.
 * Состав меняет администратор на странице проекта. */
function Members({ projectId }: { projectId: string }) {
  const { t } = useTranslation();
  const load = useCallback(() => accessApi.members(projectId), [projectId]);
  const members = useResource(load);
  const list = members.data;
  const title = list
    ? t('project_overview.members_count', { count: list.length })
    : t('project_overview.members');
  return (
    <Card name="members" title={title}>
      <LoadState loading={members.loading} error={members.error ?? ''} retry={members.refresh}>
        {list && list.length > 0 ? (
          <>
            <ul className={local.members}>
              {list.slice(0, NAMED_MEMBERS).map((member) => (
                <li key={member.username} title={member.username}>
                  <span className={member.disabled ? styles.muted : undefined}>
                    {member.display_name || member.username}
                  </span>
                  {member.disabled && (
                    <StatusBadge tone="neutral">{t('access.disabled')}</StatusBadge>
                  )}
                </li>
              ))}
            </ul>
            {list.length > NAMED_MEMBERS && (
              <p className={local.hint}>
                {t('project_overview.more_members', { count: list.length - NAMED_MEMBERS })}
              </p>
            )}
          </>
        ) : (
          <p className={local.hint}>{t('project_overview.no_members')}</p>
        )}
      </LoadState>
    </Card>
  );
}
