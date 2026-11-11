/**
 * Чистая логика страницы роя (#215): состояние исполнения, выбранный узел
 * графа и имена вместо идентификаторов. Страница только рисует результат.
 */
import { executionTone } from '@/features/access/state';
import type { AccessProject, AccessSwarm } from '@/services/brood/access';
import type { NodeStatus, PipelineRun, RunGraph, RunNode } from '@/services/brood/pipelines';
import type { SwarmRolesView } from '@/services/brood/roles';

/** Состояние исполнения роя (#206). Оно не смешивается с состоянием
 * инфраструктуры (provisioning, active, ...): у каждого свой значок. */
export type SwarmExecution = 'idle' | 'running' | 'waiting' | 'succeeded' | 'failed' | 'stopped';

/** Исполнение со старого провода прогона: рой без графа ещё не начинал,
 * waiting_human — это waiting. Незнакомое значение (сервер новее панели)
 * остаётся как есть, а не выдаётся за другое состояние. */
export function swarmExecution(run: PipelineRun | null): string {
  if (!run) return 'idle';
  return run.status === 'waiting_human' ? 'waiting' : run.status;
}

/** Завершённое исполнение дальше не опрашивать; ещё не прочитанное и
 * идущее или ждущее ответа — опрашивать. */
export function runIsLive(run: PipelineRun | null): boolean {
  return run === null || run.status === 'running' || run.status === 'waiting_human';
}

/** Тон исполнения живёт в общем словаре тонов (#254). */
export { executionTone };

/** Узлы, известные этому рою: из графа и из шагов. */
function knownNodes(run: PipelineRun | null, graph: RunGraph | null): Set<string> {
  const known = new Set<string>();
  for (const node of graph?.nodes ?? []) known.add(node.id);
  for (const step of run?.nodes ?? []) known.add(step.id);
  return known;
}

/** Выбранный узел: выбор оператора, пока такой узел есть в графе роя,
 * иначе узел, на котором стоит исполнение. */
export function selectedNodeId(
  run: PipelineRun | null,
  graph: RunGraph | null,
  chosen: string | undefined
): string | undefined {
  const known = knownNodes(run, graph);
  if (chosen !== undefined && known.has(chosen)) return chosen;
  const current = run?.currentNode;
  return current !== undefined && known.has(current) ? current : undefined;
}

/** Метка шага на таймлайне и у состояния узла: точка красится токенами
 * графа. Слово состояния рядом всегда, цвет его только дублирует. */
export type StepMark = 'pending' | 'active' | 'done' | 'failed' | 'skipped' | 'unconfirmed';

/** Метка состояния шага (#212): работающий шаг — active, завершённый —
 * done, отказ — failed (шаг закрыт без эффекта, нужен оператор),
 * остановленный — приглушённый skipped. Неизвестный исход — своя метка:
 * он не провал и не работа. Незнакомое состояние (сервер новее панели) не
 * выдаётся ни за работу, ни за провал. */
export function stepMark(status: string): StepMark {
  switch (status) {
    case 'attesting':
    case 'delivering':
    case 'inflight':
    case 'waiting':
    case 'active':
      return 'active';
    case 'answered':
    case 'completed':
    case 'done':
      return 'done';
    case 'refused':
    case 'failed':
      return 'failed';
    case 'stopped':
    case 'skipped':
      return 'skipped';
    case 'unconfirmed':
      return 'unconfirmed';
    default:
      return 'pending';
  }
}

/** Что делать оператору: refused и unconfirmed закрывают шаг, но не проход,
 * и диспетчер к нему сам не вернётся (docs/MODEL.md, «Шаг актора»). */
export type StepGuidance = 'refused' | 'unconfirmed';

/** Команда, которой администратор отпускает неподтверждённую lease
 * workspace (docs/MCP.md, `dispatch --action release`): ID операции — в
 * причине шага, панель его не разбирает из текста сервера. */
export function releaseCommand(swarmId: string, actorName: string | undefined): string {
  return `bro dispatch ${actorName || '<actor>'} --request-id <operation> --action release --swarm ${swarmId}`;
}

/** Короткий commit на экране; полный — в подсказке и копировании. Восемь
 * знаков — как короткий ID коммита в GitLab, где его будут искать. */
export function shortCommit(commit: string): string {
  return commit.slice(0, 8);
}

export interface NodeView {
  id: string;
  label: string;
  kind: string;
  /** Состояние последнего посещения; pending — узел ещё не посещался. */
  status: NodeStatus;
  outcome?: string;
  actorName?: string;
  startedAt?: string;
  finishedAt?: string;
  visits: number;
  /** Что граф поручает узлу: prompt актору, вопрос оператору. */
  prompt?: string;
  question?: string;
  /** Причина refused или unconfirmed последнего посещения (#212). */
  detail?: string;
  /** Commit, который подтвердил workspace актора последнего посещения. */
  commit?: string;
  /** Отчёт актора о последнем посещении (#233), простой текст. */
  report?: string;
  /** Что делать оператору: только у шага, на котором стоит исполнение. */
  guidance?: StepGuidance;
}

/** Сводка выбранного узла: исход, актор, причина, commit, отчёт и моменты —
 * по последнему посещению, инструкция — из графа роя. Совет оператору —
 * только когда это посещение последний шаг исполнения: отказ прошлого
 * прохода уже разрешён новым проходом. */
export function nodeView(run: PipelineRun | null, graph: RunGraph | null, id: string): NodeView {
  const steps = run?.nodes ?? [];
  const visits = steps.filter((step) => step.id === id);
  const last = visits[visits.length - 1];
  const definition = graph?.nodes?.find((node) => node.id === id);
  const status = last === steps[steps.length - 1] ? last?.status : undefined;
  return {
    id,
    label: last?.label ?? id,
    kind: definition?.kind ?? last?.kind ?? '',
    status: last?.status ?? 'pending',
    outcome: last?.outcome || undefined,
    actorName: last?.actorName || undefined,
    startedAt: last?.startedAt,
    finishedAt: last?.finishedAt,
    visits: visits.length,
    prompt: definition?.prompt || undefined,
    question: definition?.question || undefined,
    detail: last?.detail || undefined,
    commit: last?.commit || undefined,
    report: last?.report || undefined,
    guidance: status === 'refused' || status === 'unconfirmed' ? status : undefined,
  };
}

/** Строка «что сейчас» (#253): исполнение, узел, на котором оно стоит, и
 * состояние этого шага — только когда слово добавляет к исполнению
 * (фаза работы, отказ, неизвестный исход), а не повторяет его. Совет и
 * актор — у застрявшего шага, по которому проход встал. */
export interface ExecutionNow {
  execution: string;
  node?: string;
  step?: NodeStatus;
  guidance?: StepGuidance;
  actorName?: string;
}

/** Слова шага, которые повторяют исполнение, а не уточняют его. */
const quietSteps: Record<string, Set<string>> = {
  running: new Set(['active', 'pending']),
  waiting: new Set(['active', 'waiting', 'pending']),
  succeeded: new Set(['done', 'completed', 'answered']),
  failed: new Set(['failed']),
  stopped: new Set(['stopped', 'skipped']),
};

/** Состояние шага, которое уточняет исполнение: фаза работы, отказ,
 * неизвестный исход. Законченный или ещё не начатый шаг и слово, которое
 * повторяет исполнение, ничего не добавляют. */
function stepTells(execution: string, status: NodeStatus): boolean {
  if (quietSteps[execution]?.has(status)) return false;
  const mark = stepMark(status);
  return mark !== 'done' && mark !== 'pending';
}

export function executionNow(run: PipelineRun | null): ExecutionNow {
  const execution = swarmExecution(run);
  if (!run) return { execution };
  const now: ExecutionNow = { execution };
  const node = run.currentNode;
  if (!node) return now;
  now.node = node;
  const last = run.nodes[run.nodes.length - 1];
  if (!last || last.id !== node) return now;
  if (stepTells(execution, last.status)) now.step = last.status;
  if (last.status === 'refused' || last.status === 'unconfirmed') {
    now.guidance = last.status;
    if (last.actorName) now.actorName = last.actorName;
  }
  return now;
}

/** Длительность шага в миллисекундах по записанным моментам; без одного из
 * них — ничего. Конец раньше начала (часы серверов) — ноль, не минус. */
export function stepDuration(step: { startedAt?: string; finishedAt?: string }): number | undefined {
  if (!step.startedAt || !step.finishedAt) return undefined;
  const started = Date.parse(step.startedAt);
  const finished = Date.parse(step.finishedAt);
  if (Number.isNaN(started) || Number.isNaN(finished)) return undefined;
  return Math.max(0, finished - started);
}

export type DurationUnit = 's' | 'min' | 'h' | 'd';

export interface DurationPart {
  unit: DurationUnit;
  value: number;
}

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

/** Длительность словами: старшая единица, и следующая — только когда она
 * не ноль и старшая крупнее минуты. «10 min», «1 h 5 min», «2 d». */
export function durationParts(ms: number): DurationPart[] {
  if (ms < MINUTE) return [{ unit: 's', value: Math.floor(ms / 1000) }];
  if (ms < HOUR) return [{ unit: 'min', value: Math.floor(ms / MINUTE) }];
  const [major, minor, size, step]: [DurationUnit, DurationUnit, number, number] =
    ms < DAY ? ['h', 'min', HOUR, MINUTE] : ['d', 'h', DAY, HOUR];
  const parts: DurationPart[] = [{ unit: major, value: Math.floor(ms / size) }];
  const rest = Math.floor((ms % size) / step);
  if (rest > 0) parts.push({ unit: minor, value: rest });
  return parts;
}

/** Блок ролей (#253) раскрыт, когда роли ждут человека — есть роль графа
 * не ready — или рой не работает, и оператор может их менять. Пока рой
 * идёт или ждёт ответа с готовыми ролями, блок свёрнут. */
export function rolesUnfolded(view: SwarmRolesView | null): boolean {
  if (!view) return false;
  if (view.roles.some((role) => role.inGraph && role.state !== 'ready')) return true;
  return view.execution !== 'running' && view.execution !== 'waiting';
}

/** Колонки времени шага есть, только если сервер записал моменты: иначе
 * колонка была бы сплошным прочерком. */
export function hasStepTimes(run: PipelineRun | null): boolean {
  return (run?.nodes ?? []).some((step) => Boolean(step.startedAt || step.finishedAt));
}

/** Проход роя в истории шагов (#242): его первый шаг (pass), порядковый
 * номер среди проходов, задача и шаги по порядку. У сервера без проходов
 * группа одна и без pass. */
export interface PassView {
  pass?: number;
  number?: number;
  goal?: string;
  steps: RunNode[];
}

const text = (value: unknown): string | undefined =>
  typeof value === 'string' && value !== '' ? value : undefined;

/** Шаги по проходам: подряд идущие шаги одного pass — один проход. Задачу
 * сервер отдаёт только у первого шага прохода; если его в списке нет,
 * у прохода нет задачи, а не чужая. */
export function stepPasses(steps: RunNode[]): PassView[] {
  const passes: PassView[] = [];
  let number = 0;
  for (const step of steps) {
    const pass = Number.isSafeInteger(step.pass) ? step.pass : undefined;
    let last = passes[passes.length - 1];
    if (!last || last.pass !== pass) {
      last = { steps: [] };
      if (pass !== undefined) {
        number += 1;
        last.pass = pass;
        last.number = number;
      }
      passes.push(last);
    }
    const goal = pass !== undefined && step.sequence === pass ? text(step.goal) : undefined;
    if (goal !== undefined) last.goal = goal;
    last.steps.push(step);
  }
  return passes;
}

/** Задача текущего прохода: текст — из исполнения, первый шаг прохода и
 * его номер — по последнему шагу. Без задачи (до первого запуска, старый
 * сервер) — null. */
export function passGoal(
  run: PipelineRun | null
): { goal: string; pass?: number; number?: number } | null {
  const goal = text(run?.goal);
  if (!run || goal === undefined) return null;
  const passes = stepPasses(run.nodes);
  const current = passes[passes.length - 1];
  if (current?.pass === undefined) return { goal };
  return { goal, pass: current.pass, number: current.number };
}

/** Имя проекта роя: из каталога проектов, затем из исполнения того же
 * проекта; ID — только когда имени нет нигде. */
export function swarmProjectName(
  swarm: AccessSwarm,
  projects: AccessProject[] | null,
  run: PipelineRun | null
): string {
  const listed = projects?.find((project) => project.id === swarm.project_id)?.name;
  if (listed) return listed;
  if (run?.projectId === swarm.project_id && run.projectName) return run.projectName;
  return swarm.project_id;
}
