import { Fragment, useCallback, useEffect, useId, useState, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { Markdown, PlainText } from '@/components/ui/Markdown';
import { useMediaQuery } from '@/hooks/useMediaQuery';
import { useProjectStore } from '@/stores';
import { accessApi, type AccessSwarm } from '@/services/brood/access';
import {
  pipelinesApi,
  type PipelineRun,
  type RunGraph as Graph,
  type RunNode,
} from '@/services/brood/pipelines';
import { LoadState, StatusBadge, useAdministrator } from '@/features/access/shared';
import { swarmTone } from '@/features/access/state';
import { swarmCapabilities } from '@/features/access/shardState';
import { SwarmManagement } from '@/features/access/SwarmManagement';
import { CopyButton } from '@/features/oauth/components/CopyButton';
import { ObjectLink } from '@/features/objects/ObjectLink';
import type { SwarmScope } from '@/features/objects/relations';
import { RunGraph } from '@/features/pipelines/RunGraph';
import { PageHeader } from '@/features/substrate/components/PageHeader';
import { Section } from '@/features/substrate/components/form';
import { useResource } from '@/features/substrate/hooks';
import { DASH } from '@/features/substrate/format';
import { formatDateTimeValue } from '@/utils/format';
import { SWARMS_PATH } from './routes';
import { nodeRequest } from './requestState';
import { RequestCard, RequestInbox } from './SwarmRequests';
import { useSwarmRequests } from './useSwarmRequests';
import { useGraphRefetch, useSwarmPolling } from './useSwarmPolling';
import { SnapshotFacts, SwarmExecutionControl } from './SwarmControls';
import { SwarmObjects } from './SwarmObjects';
import { SwarmRoles } from './SwarmRoles';
import { executionAction, snapshotIdentity } from './swarmControl';
import {
  durationParts,
  executionNow,
  executionTone,
  hasStepTimes,
  nodeView,
  passGoal,
  releaseCommand,
  runIsLive,
  selectedNodeId,
  shortCommit,
  stepDuration,
  stepMark,
  stepPasses,
  swarmProjectName,
  type ExecutionNow,
  type NodeView,
  type StepGuidance,
} from './swarmState';
import styles from '@/features/substrate/substrate.module.scss';
import graphStyles from '@/features/pipelines/pipelines.module.scss';
import local from './SwarmPage.module.scss';

/** Высота задачи прохода в свёрнутом виде (#253): две строки по 13px/1.55. */
const GOAL_LINES_HEIGHT = 42;
/** Телефон: сведения о рое складываются под «Сведения» (#253). */
const PHONE = '(max-width: 768px)';

/** Страница роя (#215) — главный объект панели. Ключ по ID: другой рой
 * не наследует ни выбранный узел, ни загруженные данные. */
export function SwarmPage() {
  const { id = '' } = useParams<{ id: string }>();
  return <SwarmScreen key={id} id={id} />;
}

/** Владелец состояния страницы: рой, его исполнение и граф, выбор узла.
 * Исполнение и граф перечитываются, пока рой работает или ждёт ответа. */
function SwarmScreen({ id }: { id: string }) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const loadSwarm = useCallback(() => accessApi.swarm(id), [id]);
  const swarm = useResource(loadSwarm);
  const projects = useResource(accessApi.projects, { headerRefresh: false });
  // Обёртка: у роя без графа исполнение null, а null у useResource — «грузится».
  const loadRun = useCallback(async () => ({ run: await pipelinesApi.getSwarmRun(id) }), [id]);
  const execution = useResource(loadRun);
  const run = execution.data?.run ?? null;
  const loadGraph = useCallback(() => pipelinesApi.getRunGraph(id), [id]);
  const graph = useResource(run ? loadGraph : null);
  const [chosen, setChosen] = useState<string>();
  // Определение, выбранное в диалоге первого запуска: его роли — в блоке ролей.
  const [startDefinition, setStartDefinition] = useState('');
  const selected = selectedNodeId(run, graph.data, chosen);
  // Запросы оператору (#216), ответ — только в инбоксе (#259). Ответ
  // узловому запросу продвигает граф; узел, выбранный в панели, остаётся
  // выбранным: панель показывает подпись выбранного варианта, а строка
  // состояния — куда граф пошёл.
  const inbox = useSwarmRequests(id, swarm.data, (answered) => {
    if (answered.origin !== 'node') return;
    if (answered.nodeId && answered.nodeId === selected) setChosen(answered.nodeId);
    void execution.refresh();
    void graph.refresh();
  });
  const administrator = useAdministrator();
  // Один тик на страницу (#257): исполнение перечитывается, пока идёт или
  // ждёт; инбокс и роли подписаны на тот же тик; граф следует за исполнением.
  const live = runIsLive(run);
  const refreshExecution = execution.refresh;
  const tick = useCallback(() => {
    if (live) void refreshExecution();
  }, [live, refreshExecution]);
  useSwarmPolling(tick, swarm.data?.state === 'active');
  useGraphRefetch(execution.data ? run : undefined, graph.refresh);
  // Адрес роя выбирает его проект глобальным контекстом, как старые ссылки
  // с проектом: список роёв и разделы проекта открываются на нём.
  const setProject = useProjectStore((state) => state.setProject);
  const projectId = swarm.data?.project_id;
  useEffect(() => {
    if (projectId) setProject(projectId);
  }, [projectId, setProject]);

  const data = swarm.data;
  if (!data) {
    return (
      <div className={styles.page}>
        <PageHeader title={t('access.swarm')} meta={[<SwarmsLink key="list" />]} />
        <LoadState loading={swarm.loading} error={swarm.error ?? ''} retry={swarm.refresh}>
          {null}
        </LoadState>
      </div>
    );
  }
  const asked = nodeRequest(inbox.requests, selected);
  const projectName = swarmProjectName(data, projects.data, run);
  // Исполнение, которое не прочитано или не перечитывается, действия не даёт:
  // по устаревшему состоянию кнопка предложила бы не то.
  const known = execution.data !== null && !execution.error ? run : undefined;
  return (
    <SwarmView
      swarm={data}
      projectName={projectName}
      objects={<SwarmObjects swarm={data} changed={swarm.refresh} />}
      actions={
        swarmCapabilities(data, administrator).manage && (
          <SwarmExecutionControl
            swarm={data}
            projectName={projectName}
            run={run}
            action={executionAction(data, administrator, known)}
            changed={async () => {
              await Promise.all([execution.refresh(), graph.refresh(), inbox.refresh()]);
            }}
            chose={setStartDefinition}
          />
        )
      }
      refreshError={swarm.error ?? ''}
      run={run}
      runLoading={execution.loading}
      runError={execution.error ?? ''}
      retryRun={execution.refresh}
      graph={graph.data}
      graphLoading={graph.loading}
      graphError={graph.error ?? ''}
      retryGraph={graph.refresh}
      selected={selected}
      onSelect={setChosen}
      gateNode={asked?.nodeId}
      gate={asked && <RequestCard key={asked.id} model={inbox} request={asked} variant="node" />}
      requests={<RequestInbox model={inbox} onSelectNode={setChosen} />}
      roles={
        <SwarmRoles
          swarm={data}
          run={execution.data ? run : undefined}
          definition={startDefinition}
        />
      }
      management={
        <SwarmManagement
          swarm={data}
          changed={swarm.refresh}
          deleted={() => navigate(SWARMS_PATH)}
        />
      }
    />
  );
}

function SwarmsLink() {
  const { t } = useTranslation();
  return <Link to={SWARMS_PATH}>{t('access.swarms')}</Link>;
}

export interface SwarmViewProps {
  swarm: AccessSwarm;
  projectName: string;
  /** Объекты роя (#195): акторы, шаблоны и доступ ссылками с числом, под
   * сведениями. */
  objects?: ReactNode;
  /** Действие исполнения роя (#218): запуск или остановка, рядом со
   * строкой состояния — там, где совет говорит остановить и запустить. */
  actions?: ReactNode;
  /** Сбой перечитывания уже показанного роя: данные остаются, причина видна. */
  refreshError?: string;
  /** Исполнение роя; null — графа у роя нет. */
  run: PipelineRun | null;
  runLoading: boolean;
  runError: string;
  retryRun: () => Promise<void>;
  graph: Graph | null;
  graphLoading: boolean;
  graphError: string;
  retryGraph: () => Promise<void>;
  selected?: string;
  onSelect: (node: string) => void;
  /** Узел запроса оператору и его карточка (#216): путь к форме ответа в
   * инбоксе или ответ (#259). */
  gateNode?: string;
  gate?: ReactNode;
  /** Инбокс запросов оператору: сразу под строкой состояния, выше графа. */
  requests?: ReactNode;
  /** Роли графа и их акторы (#241): свёрнуты под графом, перед историей
   * шагов; у роя без графа — под инбоксом. */
  roles?: ReactNode;
  /** Доступ и жизненный цикл: в конце страницы, после работы роя. */
  management?: ReactNode;
}

/** Рой целиком (#253): первый экран отвечает «что сейчас и что от меня
 * нужно» — сведения одной строкой под заголовком, строка состояния
 * исполнения с советом застрявшего шага и действием (#218), задача прохода
 * (#242) в две строки, открытые запросы оператору (#216); дальше граф и
 * панель выбранного узла в две колонки, свёрнутые роли (#241), история
 * шагов по проходам, затем доступ и удаление. Подпись к графу — личность
 * снимка роя, а не текущее определение каталога. */
export function SwarmView({
  swarm,
  projectName,
  objects,
  actions,
  refreshError,
  run,
  runLoading,
  runError,
  retryRun,
  graph,
  graphLoading,
  graphError,
  retryGraph,
  selected,
  onSelect,
  gateNode,
  gate,
  requests,
  roles,
  management,
}: SwarmViewProps) {
  const { t } = useTranslation();
  const identity = snapshotIdentity(run);
  const current = passGoal(run);
  const scope: SwarmScope = { project: swarm.project_id, swarm: swarm.id };
  return (
    <div className={styles.page}>
      <PageHeader title={swarm.name} meta={[<SwarmsLink key="list" />]} />
      <SwarmMeta swarm={swarm} projectName={projectName} />
      {objects}
      {refreshError && (
        <p className={`${styles.notice} ${styles.operationFailure}`} role="alert">
          {refreshError}
        </p>
      )}
      <NowBlock
        run={run}
        swarmId={swarm.id}
        loading={runLoading}
        error={runError}
        retry={retryRun}
        selected={selected}
        onSelect={onSelect}
        actions={actions}
        goal={current}
      />
      {requests}
      {run && (
        <>
          <div className={local.work}>
            <Section
              title={t('swarm_page.graph')}
              action={
                identity && (
                  <div className={local.graphCaption}>
                    <SnapshotFacts identity={identity} />
                  </div>
                )
              }
            >
              <LoadState loading={graphLoading} error={graphError} retry={retryGraph}>
                {graph?.svg ? (
                  <RunGraph
                    svg={graph.svg}
                    label={t('swarm_page.graph_label', { name: swarm.name })}
                    activeNode={run.currentNode}
                    selectedNode={selected}
                    onSelect={onSelect}
                  />
                ) : (
                  <p className={local.hint}>{t('pipelines.graph_empty')}</p>
                )}
              </LoadState>
            </Section>
            <div className={local.workAside}>
              <Section title={t('swarm_page.node')}>
                {selected ? (
                  <NodeDetails
                    node={nodeView(run, graph, selected)}
                    gate={selected === gateNode ? gate : null}
                    scope={scope}
                  />
                ) : (
                  <p className={local.hint}>{t('swarm_page.node_hint')}</p>
                )}
              </Section>
            </div>
          </div>
          {roles}
          <Section title={t('swarm_page.steps')} hint={t('swarm_page.steps_hint')}>
            <StepHistory
              steps={run.nodes}
              selected={selected}
              onSelect={onSelect}
              times={hasStepTimes(run)}
              shownPass={current?.pass}
              scope={scope}
            />
          </Section>
        </>
      )}
      {!run && roles}
      {management}
    </div>
  );
}

/** Сведения о рое одной строкой под заголовком: проект ссылкой, владелец и
 * роль именами, полный ID с копированием, пространство в Substrate и
 * состояние инфраструктуры бейджем; не активная инфраструктура — строкой
 * тревоги под ними. На телефоне строка складывается под «Сведения». */
function SwarmMeta({ swarm, projectName }: { swarm: AccessSwarm; projectName: string }) {
  const { t } = useTranslation();
  const phone = useMediaQuery(PHONE);
  const state = t(`access.states.${swarm.state}`, { defaultValue: swarm.state });
  const facts = (
    <ul className={local.metaList}>
      <li>
        <span className={local.metaLabel}>{t('swarm_page.project')}</span>
        <Link to={`/projects/${encodeURIComponent(swarm.project_id)}`}>{projectName}</Link>
      </li>
      <li>
        <span className={local.metaLabel}>{t('swarm_page.owner')}</span>
        {swarm.owner_username}
      </li>
      <li>
        <span className={local.metaLabel}>{t('swarm_page.your_role')}</span>
        {t(`access.swarm_roles.${swarm.role}`, { defaultValue: swarm.role })}
      </li>
      <li>
        <span className={local.metaLabel}>{t('swarm_page.id')}</span>
        <code className={local.id}>{swarm.id}</code>
        <CopyButton value={swarm.id} label={t('swarm_page.copy_id')} />
      </li>
      <li>
        <span className={local.metaLabel}>{t('swarm_page.space')}</span>
        <code className={local.id}>{swarm.atespace_name}</code>
      </li>
      <li>
        <span className={local.metaLabel}>{t('swarm_page.infrastructure_label')}</span>
        <StatusBadge tone={swarmTone(swarm.state)}>{state}</StatusBadge>
      </li>
    </ul>
  );
  return (
    <div className={local.meta} data-swarm-meta="">
      {phone ? (
        <details className={local.metaFold}>
          <summary className={local.metaSummary}>{t('swarm_page.details')}</summary>
          {facts}
        </details>
      ) : (
        facts
      )}
      {swarm.state !== 'active' && (
        <p className={styles.notice} data-infrastructure-alert="">
          {t('swarm_page.infrastructure_alert', { state })}
        </p>
      )}
    </div>
  );
}

/** Первый блок страницы (#253): одна строка состояния — исполнение, узел,
 * на котором оно стоит (кнопкой — тем же выбором, что на графе), и слово
 * шага, когда оно уточняет; рядом действие (#218). Под строкой — совет
 * застрявшему шагу (#238) или пояснение состояния, затем задача текущего
 * прохода (#242) в две строки с развёртыванием. */
function NowBlock({
  run,
  swarmId,
  loading,
  error,
  retry,
  selected,
  onSelect,
  actions,
  goal,
}: {
  run: PipelineRun | null;
  swarmId: string;
  loading: boolean;
  error: string;
  retry: () => Promise<void>;
  selected?: string;
  onSelect: (node: string) => void;
  actions?: ReactNode;
  goal: ReturnType<typeof passGoal>;
}) {
  const { t } = useTranslation();
  const now = executionNow(run);
  return (
    <section className={local.now} aria-label={t('swarm_page.now_label')} data-swarm-now="">
      <div className={local.nowHead}>
        <LoadState loading={loading} error={error} retry={retry}>
          <ExecutionLine now={now} hasGraph={run !== null} selected={selected} onSelect={onSelect} />
        </LoadState>
      </div>
      {actions && (
        <div className={local.nowActions} data-swarm-action="">
          {actions}
        </div>
      )}
      {!loading && !error && now.guidance && (
        <StepAdvice guidance={now.guidance} swarmId={swarmId} actorName={now.actorName} />
      )}
      {goal && <PassGoal {...goal} />}
    </section>
  );
}

/** Строка состояния словами: остановленный рой и рой, ждущий ответа,
 * различаются значком, текстом и пояснением; у застрявшего шага пояснение
 * заменяет совет, чтобы «граф идёт» не спорило с «исход неизвестен». */
function ExecutionLine({
  now,
  hasGraph,
  selected,
  onSelect,
}: {
  now: ExecutionNow;
  hasGraph: boolean;
  selected?: string;
  onSelect: (node: string) => void;
}) {
  const { t } = useTranslation();
  return (
    <div className={local.nowText}>
      <p className={local.nowLine}>
        <StatusBadge tone={executionTone(now.execution)}>
          {t(`swarm_page.execution_states.${now.execution}`, { defaultValue: now.execution })}
        </StatusBadge>
        {now.node && (
          <>
            <Dot />
            <span className={local.nowNode}>
              {t('swarm_page.now_node')}{' '}
              <button
                type="button"
                className={local.nodeButton}
                aria-pressed={selected === now.node}
                onClick={() => onSelect(now.node ?? '')}
              >
                {now.node}
              </button>
            </span>
          </>
        )}
        {now.step && (
          <>
            <Dot />
            <NodeState status={now.step} />
          </>
        )}
      </p>
      {!now.guidance && (
        <p className={local.nowHint}>
          {t(`swarm_page.execution_hints.${hasGraph ? now.execution : 'no_graph'}`, {
            defaultValue: '',
          })}
        </p>
      )}
    </div>
  );
}

/** Разделитель частей строки: точка, как в meta-строке заголовка. */
function Dot() {
  return (
    <span className={local.nowDot} aria-hidden="true">
      ·
    </span>
  );
}

/** Задача текущего прохода (#242) — под строкой состояния, в две строки с
 * развёртыванием: markdown без HTML (#252), переводы строк сохранены. */
function PassGoal({ goal, pass, number }: { goal: string; pass?: number; number?: number }) {
  const { t } = useTranslation();
  const labelId = useId();
  return (
    <section className={local.goal} aria-labelledby={labelId} data-pass-goal="">
      <p id={labelId} className={local.goalPass}>
        {pass === undefined
          ? t('swarm_page.pass_goal.current')
          : t('swarm_page.pass_goal.pass', { number, step: pass })}
      </p>
      <Markdown
        className={local.goalText}
        data-goal-text=""
        text={goal}
        collapsible
        collapsedHeight={GOAL_LINES_HEIGHT}
      />
    </section>
  );
}

/** Точка состояния шага: метка stepMark, красится токенами графа. */
function StepDot({ status }: { status: string }) {
  return <span className={graphStyles[`timeline-${stepMark(status)}`]} aria-hidden="true" />;
}

type Translate = (key: string, options?: { defaultValue?: string; count?: number }) => string;

/** Состояние шага словом; незнакомое (сервер новее панели) — как есть. */
function stateWord(t: Translate, status: string) {
  return t(`swarm_page.node_states.${status}`, { defaultValue: status });
}

/** Состояние узла словами рядом с точкой: цвет точки только дублирует слово. */
function NodeState({ status }: { status: string }) {
  const { t } = useTranslation();
  return (
    <span className={graphStyles.nodeState}>
      <StepDot status={status} />
      {stateWord(t, status)}
    </span>
  );
}

/** Подтверждённый commit шага: короткий на экране, полный — в подсказке и
 * в копировании. */
function StepCommit({ commit }: { commit: string }) {
  const { t } = useTranslation();
  return (
    <span className={local.commit}>
      <code className={local.id} title={commit}>
        {shortCommit(commit)}
      </code>
      <CopyButton value={commit} label={t('swarm_page.copy_commit')} />
    </span>
  );
}

/** Что делать с шагом, на котором встал проход (docs/MODEL.md, «Шаг
 * актора»): refused — устранить причину, остановить рой и запустить новый
 * проход; unconfirmed — решение оператора, а неподтверждённую lease
 * workspace отпускает администратор по ID операции из причины. Совет
 * стоит открыто; как отпустить lease — свёрнуто под своей строкой: это
 * дело администратора и только при занятом workspace. */
function StepAdvice({
  guidance,
  swarmId,
  actorName,
}: {
  guidance: StepGuidance;
  swarmId: string;
  actorName?: string;
}) {
  const { t } = useTranslation();
  return (
    <div className={local.guidance} data-guidance={guidance} role="note">
      <p>{t(`swarm_page.step_guidance.${guidance}`)}</p>
      {guidance === 'unconfirmed' && (
        <details className={local.release}>
          <summary>{t('swarm_page.step_guidance.release_title')}</summary>
          <p>{t('swarm_page.step_guidance.release')}</p>
          <code className={local.command}>{releaseCommand(swarmId, actorName)}</code>
        </details>
      )}
    </div>
  );
}

/** Актор шага -- ссылкой на его панель в рое (#195); без имени -- прочерк. */
function StepActor({ name, scope }: { name?: string; scope: SwarmScope }) {
  if (!name) return <>{DASH}</>;
  return <ObjectLink object={{ kind: 'actor', ...scope, name }} />;
}

/** Момент шага с датой и временем; не записанный — прочерк. */
function moment(value: string | undefined, language: string): string {
  return (value && formatDateTimeValue(value, language)) || DASH;
}

function kindName(t: Translate, kind: string) {
  return kind === '' ? '' : t(`pipelines.kind.${kind}`, { defaultValue: kind });
}

/** Длительность словами из частей: «10 min», «1 h 5 min». */
function durationWords(t: Translate, ms: number): string {
  return durationParts(ms)
    .map((part) => t(`swarm_page.duration.${part.unit}`, { count: part.value }))
    .join(' ');
}

/** Сколько шёл шаг — одним словом; полные моменты в подсказке. Шаг без
 * конца называет начало. */
function StepSpan({ step }: { step: RunNode }) {
  const { t, i18n } = useTranslation();
  const ms = stepDuration(step);
  if (ms === undefined) {
    if (!step.startedAt) return null;
    return (
      <div>
        <dt>{t('swarm_page.started')}</dt>
        <dd>{moment(step.startedAt, i18n.language)}</dd>
      </div>
    );
  }
  const title = `${t('swarm_page.started')} ${moment(step.startedAt, i18n.language)}, ${t(
    'swarm_page.finished'
  )} ${moment(step.finishedAt, i18n.language)}`;
  return (
    <div>
      <dt>{t('swarm_page.duration_label')}</dt>
      <dd>
        <span title={title}>{durationWords(t, ms)}</span>
      </dd>
    </div>
  );
}

/** Выбранный узел: что поручает граф, исход, актор, причина, commit, отчёт
 * и моменты последнего посещения. Совет застрявшему шагу стоит в первом
 * блоке страницы, здесь не повторяется. Запрос оператору этого узла —
 * карточкой без формы (#259): форма одна, в инбоксе, сюда — кнопка к ней,
 * а после ответа — подпись выбранного варианта. */
function NodeDetails({
  node,
  gate,
  scope,
}: {
  node: NodeView;
  gate?: ReactNode;
  scope: SwarmScope;
}) {
  const { t, i18n } = useTranslation();
  const rows: Array<{ label: string; value: ReactNode }> = [];
  if (node.prompt) {
    rows.push({
      label: t('swarm_page.prompt'),
      value: <Markdown className={local.instruction} text={node.prompt} collapsible />,
    });
  }
  if (node.question) {
    rows.push({
      label: t('swarm_page.question'),
      value: <Markdown className={local.instruction} text={node.question} />,
    });
  }
  if (node.kind === 'actor') {
    rows.push({
      label: t('swarm_page.actor'),
      value: <StepActor name={node.actorName} scope={scope} />,
    });
  }
  if (node.visits > 0) {
    rows.push({ label: t('swarm_page.outcome'), value: node.outcome ?? DASH });
  }
  if (node.detail) {
    rows.push({
      label: t('swarm_page.reason'),
      value: <PlainText className={local.instruction} text={node.detail} />,
    });
  }
  if (node.commit) {
    rows.push({ label: t('swarm_page.commit'), value: <StepCommit commit={node.commit} /> });
  }
  if (node.report) {
    rows.push({
      label: t('swarm_page.report'),
      value: <Markdown className={local.instruction} text={node.report} collapsible />,
    });
  }
  if (node.startedAt) {
    rows.push({ label: t('swarm_page.started'), value: moment(node.startedAt, i18n.language) });
  }
  if (node.finishedAt) {
    rows.push({
      label: t('swarm_page.finished'),
      value: moment(node.finishedAt, i18n.language),
    });
  }
  return (
    <div className={local.nodeCard}>
      <p className={local.nodeHead}>
        {node.label}
        <span className={local.kind}>{kindName(t, node.kind)}</span>
        <NodeState status={node.visits === 0 ? 'unvisited' : node.status} />
      </p>
      <dl className={styles.details}>
        {rows.map((row) => (
          <Fragment key={row.label}>
            <dt>{row.label}</dt>
            <dd>{row.value}</dd>
          </Fragment>
        ))}
      </dl>
      {node.visits > 1 && (
        <p className={local.hint}>{t('swarm_page.entered', { count: node.visits })}</p>
      )}
      {gate}
    </div>
  );
}

/** История шагов — таймлайн посещений по порядку. Имя узла выбирает его
 * на графе; шаги выбранного узла подсвечены. У шага — состояние словом,
 * причина, подтверждённый commit и свёрнутый отчёт, если сервер их отдал;
 * длительность — только если сервер записал моменты. Шаги проходов (#242)
 * идут под заголовком своего прохода с его задачей; задачу прохода,
 * показанную в первом блоке страницы (shownPass), история не повторяет
 * (#253). У сервера без проходов список один. */
function StepHistory({
  steps,
  selected,
  onSelect,
  times,
  shownPass,
  scope,
}: {
  steps: RunNode[];
  selected?: string;
  onSelect: (node: string) => void;
  times: boolean;
  shownPass?: number;
  scope: SwarmScope;
}) {
  const { t } = useTranslation();
  if (steps.length === 0) return <p className={local.hint}>{t('swarm_page.steps_empty')}</p>;
  const passes = stepPasses(steps);
  // Номер шага — его sequence, у старого сервера без него — место в истории.
  const numbers = new Map(steps.map((step, index) => [step, step.sequence ?? index + 1]));
  const list = (items: RunNode[]) => (
    <ol className={graphStyles.timeline}>
      {items.map((step) => {
        const number = numbers.get(step) ?? 0;
        return (
          <StepItem
            key={number}
            step={step}
            number={number}
            selected={selected}
            onSelect={onSelect}
            times={times}
            scope={scope}
          />
        );
      })}
    </ol>
  );
  if (passes.every((pass) => pass.pass === undefined)) return list(steps);
  return (
    <div className={local.passes}>
      {passes.map((pass, index) =>
        pass.pass === undefined ? (
          <Fragment key={`steps-${index}`}>{list(pass.steps)}</Fragment>
        ) : (
          <PassSteps
            key={pass.pass}
            pass={pass.pass}
            number={pass.number}
            goal={pass.pass === shownPass ? undefined : pass.goal}
            count={pass.steps.length}
            open={index === passes.length - 1}
          >
            {list(pass.steps)}
          </PassSteps>
        )
      )}
    </div>
  );
}

/** Проход в истории шагов (#253): заголовок с номером и первым шагом и
 * число шагов — в summary; задача прохода в две строки с развёртыванием и
 * его шаги — внутри. Прошлые проходы свёрнуты: это история истории;
 * текущий (последний) раскрыт, его задача — в первом блоке страницы.
 * Нажатие оператора React не перебивает. */
function PassSteps({
  pass,
  number,
  goal,
  count,
  open,
  children,
}: {
  pass: number;
  number?: number;
  goal?: string;
  count: number;
  open: boolean;
  children: ReactNode;
}) {
  const { t } = useTranslation();
  const titleId = useId();
  return (
    <section className={local.pass} aria-labelledby={titleId} data-pass={pass}>
      <details open={open} className={local.passFold}>
        <summary className={local.passSummary}>
          <h3 id={titleId} className={local.passTitle}>
            {t('swarm_page.pass_goal.pass', { number, step: pass })}
          </h3>
          <span className={local.foldMeta}>{t('swarm_page.pass_steps', { count })}</span>
        </summary>
        <div className={local.passBody}>
          {goal && (
            <div className={local.passGoal}>
              <Markdown
                className={local.goalText}
                data-goal-text=""
                text={goal}
                collapsible
                collapsedHeight={GOAL_LINES_HEIGHT}
              />
            </div>
          )}
          {children}
        </div>
      </details>
    </section>
  );
}

/** Шаг истории: номер, узел кнопкой, вид и состояние словом, затем актор,
 * исход, причина, подтверждённый commit и длительность, свёрнутый отчёт. */
function StepItem({
  step,
  number,
  selected,
  onSelect,
  times,
  scope,
}: {
  step: RunNode;
  number: number;
  selected?: string;
  onSelect: (node: string) => void;
  times: boolean;
  scope: SwarmScope;
}) {
  const { t } = useTranslation();
  const chosen = step.id === selected;
  const facts: Array<{ label: string; value: ReactNode }> = [];
  if (step.kind === 'actor') {
    facts.push({
      label: t('swarm_page.actor'),
      value: <StepActor name={step.actorName} scope={scope} />,
    });
  }
  if (step.outcome) facts.push({ label: t('swarm_page.outcome'), value: step.outcome });
  if (step.detail) facts.push({ label: t('swarm_page.reason'), value: step.detail });
  if (step.commit) {
    facts.push({ label: t('swarm_page.commit'), value: <StepCommit commit={step.commit} /> });
  }
  return (
    <li
      className={[graphStyles['timeline-node'], local.step, chosen ? local.stepSelected : '']
        .filter(Boolean)
        .join(' ')}
    >
      <StepDot status={step.status} />
      <div className={local.stepBody}>
        <div className={local.stepTitle}>
          <span className={local.stepNumber}>#{number}</span>
          <button
            type="button"
            className={local.nodeButton}
            aria-pressed={chosen}
            onClick={() => onSelect(step.id)}
          >
            {step.label}
          </button>
          <span className={local.kind}>{kindName(t, step.kind)}</span>
          <span>{stateWord(t, step.status)}</span>
        </div>
        {(facts.length > 0 || (times && step.startedAt)) && (
          <dl className={local.stepFacts}>
            {facts.map((fact) => (
              <div key={fact.label}>
                <dt>{fact.label}</dt>
                <dd>{fact.value}</dd>
              </div>
            ))}
            {times && <StepSpan step={step} />}
          </dl>
        )}
        {step.report && (
          <details className={local.stepReport}>
            <summary>{t('swarm_page.report')}</summary>
            <Markdown className={local.instruction} text={step.report} />
          </details>
        )}
      </div>
    </li>
  );
}
