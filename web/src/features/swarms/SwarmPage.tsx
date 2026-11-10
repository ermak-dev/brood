import { Fragment, useCallback, useEffect, useId, useState, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, useNavigate, useParams } from 'react-router-dom';
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
import { SwarmRoles } from './SwarmRoles';
import { executionAction, snapshotIdentity } from './swarmControl';
import {
  executionTone,
  hasStepTimes,
  nodeView,
  passGoal,
  releaseCommand,
  runIsLive,
  selectedNodeId,
  shortCommit,
  stepMark,
  stepPasses,
  swarmExecution,
  swarmProjectName,
  type NodeView,
  type StepGuidance,
} from './swarmState';
import styles from '@/features/substrate/substrate.module.scss';
import graphStyles from '@/features/pipelines/pipelines.module.scss';
import local from './SwarmPage.module.scss';

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
  // Запросы оператору (#216). Ответ узловому запросу продвигает граф;
  // ответ, данный у узла, оставляет выбор на нём, рядом с подтверждением.
  const inbox = useSwarmRequests(id, swarm.data, (answered, place) => {
    if (answered.origin !== 'node') return;
    if (place === 'node' && answered.nodeId) setChosen(answered.nodeId);
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
  const selected = selectedNodeId(run, graph.data, chosen);
  const asked = nodeRequest(inbox.requests, selected);
  const projectName = swarmProjectName(data, projects.data, run);
  // Исполнение, которое не прочитано или не перечитывается, действия не даёт:
  // по устаревшему состоянию кнопка предложила бы не то.
  const known = execution.data !== null && !execution.error ? run : undefined;
  return (
    <SwarmView
      swarm={data}
      projectName={projectName}
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
  /** Действие исполнения роя в шапке: запуск или остановка (#218). */
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
  /** Узел запроса оператору и его карточка (#216): форма ответа или ответ. */
  gateNode?: string;
  gate?: ReactNode;
  /** Инбокс запросов оператору: под состояниями, выше графа. */
  requests?: ReactNode;
  /** Роли графа и их акторы (#241): под инбоксом, выше графа. */
  roles?: ReactNode;
  /** Доступ и жизненный цикл: в конце страницы, после работы роя. */
  management?: ReactNode;
}

/** Рой целиком: сведения, два раздельных состояния и задача текущего
 * прохода (#242), запросы оператору (#216), граф сверху вниз, выбранный
 * узел и история шагов по проходам, затем доступ и удаление. Запуск и
 * остановка (#218) — в действиях заголовка; рядом с графом — личность
 * снимка роя, а не текущее определение каталога. */
export function SwarmView({
  swarm,
  projectName,
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
  return (
    <div className={styles.page}>
      <PageHeader title={swarm.name} meta={[<SwarmsLink key="list" />]} actions={actions} />
      <SwarmFacts swarm={swarm} projectName={projectName} />
      {refreshError && (
        <p className={`${styles.notice} ${styles.operationFailure}`} role="alert">
          {refreshError}
        </p>
      )}
      <div className={local.states}>
        <StateCard label={t('swarm_page.infrastructure')}>
          <StatusBadge tone={swarmTone(swarm.state)}>
            {t(`access.states.${swarm.state}`, { defaultValue: swarm.state })}
          </StatusBadge>
          <p className={local.stateText}>
            {t('swarm_page.atespace')}: <span className={local.id}>{swarm.atespace_name}</span>
          </p>
        </StateCard>
        <StateCard label={t('swarm_page.execution')}>
          <LoadState loading={runLoading} error={runError} retry={retryRun}>
            <ExecutionState run={run} selected={selected} onSelect={onSelect} />
          </LoadState>
        </StateCard>
        {current && <PassGoal {...current} />}
      </div>
      {requests}
      {roles}
      {run && (
        <>
          <Section
            title={t('swarm_page.graph')}
            hint={t('swarm_page.graph_hint', { name: run.name })}
          >
            {identity && <SnapshotFacts identity={identity} />}
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
          <Section title={t('swarm_page.node')}>
            {selected ? (
              <NodeDetails
                node={nodeView(run, graph, selected)}
                swarmId={swarm.id}
                gate={selected === gateNode ? gate : null}
              />
            ) : (
              <p className={local.hint}>{t('swarm_page.node_hint')}</p>
            )}
          </Section>
          <Section title={t('swarm_page.steps')} hint={t('swarm_page.steps_hint')}>
            <StepHistory
              steps={run.nodes}
              selected={selected}
              onSelect={onSelect}
              times={hasStepTimes(run)}
            />
          </Section>
        </>
      )}
      {management}
    </div>
  );
}

/** Проект — ссылкой на его страницу, владелец и роль — именами, полный ID
 * роя — отдельным элементом с копированием. */
function SwarmFacts({ swarm, projectName }: { swarm: AccessSwarm; projectName: string }) {
  const { t } = useTranslation();
  return (
    <dl className={local.facts}>
      <div>
        <dt>{t('swarm_page.project')}</dt>
        <dd>
          <Link to={`/projects/${encodeURIComponent(swarm.project_id)}`}>{projectName}</Link>
        </dd>
      </div>
      <div>
        <dt>{t('swarm_page.owner')}</dt>
        <dd>{swarm.owner_username}</dd>
      </div>
      <div>
        <dt>{t('swarm_page.your_role')}</dt>
        <dd>{t(`access.swarm_roles.${swarm.role}`, { defaultValue: swarm.role })}</dd>
      </div>
      <div className={local.factWide}>
        <dt>{t('swarm_page.id')}</dt>
        <dd>
          <code className={local.id}>{swarm.id}</code>
          <CopyButton value={swarm.id} label={t('swarm_page.copy_id')} />
        </dd>
      </div>
    </dl>
  );
}

function StateCard({ label, children }: { label: string; children: ReactNode }) {
  const labelId = useId();
  return (
    <section className={local.state} aria-labelledby={labelId}>
      <p id={labelId} className={local.stateLabel}>
        {label}
      </p>
      {children}
    </section>
  );
}

/** Задача текущего прохода (#242) — на всю ширину под состояниями: текст
 * оператора как набран, переводы строк сохранены, разметка — текстом. */
function PassGoal({ goal, pass, number }: { goal: string; pass?: number; number?: number }) {
  const { t } = useTranslation();
  const labelId = useId();
  return (
    <section
      className={`${local.state} ${local.passGoal}`}
      aria-labelledby={labelId}
      data-pass-goal=""
    >
      <p id={labelId} className={local.stateLabel}>
        {t('swarm_page.pass_goal.label')}
      </p>
      {pass !== undefined && (
        <p className={local.stateText}>{t('swarm_page.pass_goal.pass', { number, step: pass })}</p>
      )}
      <p className={local.goalText} data-goal-text="">
        {goal}
      </p>
    </section>
  );
}

/** Состояние исполнения словами: остановленный рой и рой, ждущий ответа,
 * различаются значком, текстом и пояснением. Текущий узел выбирается
 * кнопкой — тем же выбором, что и на графе. */
function ExecutionState({
  run,
  selected,
  onSelect,
}: {
  run: PipelineRun | null;
  selected?: string;
  onSelect: (node: string) => void;
}) {
  const { t } = useTranslation();
  const execution = swarmExecution(run);
  const current = run?.currentNode;
  return (
    <>
      <StatusBadge tone={executionTone(execution)}>
        {t(`swarm_page.execution_states.${execution}`, { defaultValue: execution })}
      </StatusBadge>
      <p className={local.stateText}>
        {t(`swarm_page.execution_hints.${run ? execution : 'no_graph'}`, { defaultValue: '' })}
      </p>
      {current && (
        <p className={local.stateText}>
          {t('swarm_page.current_node')}:{' '}
          <button
            type="button"
            className={local.nodeButton}
            aria-pressed={selected === current}
            onClick={() => onSelect(current)}
          >
            {current}
          </button>
        </p>
      )}
    </>
  );
}

/** Точка состояния шага: метка stepMark, красится токенами графа. */
function StepDot({ status }: { status: string }) {
  return <span className={graphStyles[`timeline-${stepMark(status)}`]} aria-hidden="true" />;
}

type Translate = (key: string, options?: { defaultValue?: string }) => string;

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
 * workspace отпускает администратор по ID операции из причины. */
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
        <>
          <p>{t('swarm_page.step_guidance.release')}</p>
          <code className={local.command}>{releaseCommand(swarmId, actorName)}</code>
        </>
      )}
    </div>
  );
}

/** Момент шага с датой и временем; не записанный — прочерк. */
function moment(value: string | undefined, language: string): string {
  return (value && formatDateTimeValue(value, language)) || DASH;
}

function kindName(t: Translate, kind: string) {
  return kind === '' ? '' : t(`pipelines.kind.${kind}`, { defaultValue: kind });
}

/** Выбранный узел: что поручает граф, исход, актор, причина, commit, отчёт
 * и моменты последнего посещения; у шага, на котором встал проход, — что
 * делать. Вопрос оператору этого узла — форма ответа здесь же. */
function NodeDetails({
  node,
  swarmId,
  gate,
}: {
  node: NodeView;
  swarmId: string;
  gate?: ReactNode;
}) {
  const { t, i18n } = useTranslation();
  const rows: Array<{ label: string; value: ReactNode }> = [];
  if (node.prompt) {
    rows.push({
      label: t('swarm_page.prompt'),
      value: <p className={local.instruction}>{node.prompt}</p>,
    });
  }
  if (node.question) {
    rows.push({
      label: t('swarm_page.question'),
      value: <p className={local.instruction}>{node.question}</p>,
    });
  }
  if (node.kind === 'actor') {
    rows.push({ label: t('swarm_page.actor'), value: node.actorName ?? DASH });
  }
  if (node.visits > 0) {
    rows.push({ label: t('swarm_page.outcome'), value: node.outcome ?? DASH });
  }
  if (node.detail) {
    rows.push({
      label: t('swarm_page.reason'),
      value: <p className={local.instruction}>{node.detail}</p>,
    });
  }
  if (node.commit) {
    rows.push({ label: t('swarm_page.commit'), value: <StepCommit commit={node.commit} /> });
  }
  if (node.report) {
    rows.push({
      label: t('swarm_page.report'),
      value: <p className={local.instruction}>{node.report}</p>,
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
  rows.push({ label: t('swarm_page.visits'), value: String(node.visits) });
  return (
    <div className={local.nodeCard}>
      <p className={local.nodeHead}>
        {node.label}
        <span className={local.kind}>{kindName(t, node.kind)}</span>
        <NodeState status={node.visits === 0 ? 'unvisited' : node.status} />
      </p>
      {node.guidance && (
        <StepAdvice guidance={node.guidance} swarmId={swarmId} actorName={node.actorName} />
      )}
      <dl className={styles.details}>
        {rows.map((row) => (
          <Fragment key={row.label}>
            <dt>{row.label}</dt>
            <dd>{row.value}</dd>
          </Fragment>
        ))}
      </dl>
      {gate}
    </div>
  );
}

/** История шагов — таймлайн посещений по порядку. Имя узла выбирает его
 * на графе; шаги выбранного узла подсвечены. У шага — состояние словом,
 * причина, подтверждённый commit и свёрнутый отчёт, если сервер их отдал;
 * моменты — только если сервер их записал. Шаги проходов (#242) идут под
 * заголовком своего прохода с его задачей; у сервера без проходов список
 * один. */
function StepHistory({
  steps,
  selected,
  onSelect,
  times,
}: {
  steps: RunNode[];
  selected?: string;
  onSelect: (node: string) => void;
  times: boolean;
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
          <PassSteps key={pass.pass} pass={pass.pass} number={pass.number} goal={pass.goal}>
            {list(pass.steps)}
          </PassSteps>
        )
      )}
    </div>
  );
}

/** Проход в истории шагов: номер, первый шаг и задача прохода над его шагами. */
function PassSteps({
  pass,
  number,
  goal,
  children,
}: {
  pass: number;
  number?: number;
  goal?: string;
  children: ReactNode;
}) {
  const { t } = useTranslation();
  const titleId = useId();
  return (
    <section className={local.pass} aria-labelledby={titleId} data-pass={pass}>
      <div className={local.passHead}>
        <h4 id={titleId} className={local.passTitle}>
          {t('swarm_page.pass_goal.pass', { number, step: pass })}
        </h4>
        {goal && (
          <p className={local.goalText} data-goal-text="">
            {goal}
          </p>
        )}
      </div>
      {children}
    </section>
  );
}

/** Шаг истории: номер, узел кнопкой, вид и состояние словом, затем актор,
 * исход, причина, подтверждённый commit и моменты, свёрнутый отчёт. */
function StepItem({
  step,
  number,
  selected,
  onSelect,
  times,
}: {
  step: RunNode;
  number: number;
  selected?: string;
  onSelect: (node: string) => void;
  times: boolean;
}) {
  const { t, i18n } = useTranslation();
  const chosen = step.id === selected;
  const facts: Array<{ label: string; value: ReactNode }> = [];
  if (step.kind === 'actor') {
    facts.push({ label: t('swarm_page.actor'), value: step.actorName || DASH });
  }
  if (step.outcome) facts.push({ label: t('swarm_page.outcome'), value: step.outcome });
  if (step.detail) facts.push({ label: t('swarm_page.reason'), value: step.detail });
  if (step.commit) {
    facts.push({ label: t('swarm_page.commit'), value: <StepCommit commit={step.commit} /> });
  }
  if (times) {
    facts.push({
      label: t('swarm_page.started'),
      value: moment(step.startedAt, i18n.language),
    });
    facts.push({
      label: t('swarm_page.finished'),
      value: moment(step.finishedAt, i18n.language),
    });
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
        {facts.length > 0 && (
          <dl className={local.stepFacts}>
            {facts.map((fact) => (
              <div key={fact.label}>
                <dt>{fact.label}</dt>
                <dd>{fact.value}</dd>
              </div>
            ))}
          </dl>
        )}
        {step.report && (
          <details className={local.stepReport}>
            <summary>{t('swarm_page.report')}</summary>
            <p className={local.instruction}>{step.report}</p>
          </details>
        )}
      </div>
    </li>
  );
}
