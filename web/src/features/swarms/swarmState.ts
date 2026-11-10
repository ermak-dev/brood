/**
 * Чистая логика страницы роя (#215): состояние исполнения, выбранный узел
 * графа и имена вместо идентификаторов. Страница только рисует результат.
 */
import type { BadgeTone } from '@/features/access/state';
import type { AccessProject, AccessSwarm } from '@/services/brood/access';
import type { NodeStatus, PipelineRun, RunGraph } from '@/services/brood/pipelines';

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

/** Остановленный и ждущий ответа рой различимы и цветом, и текстом:
 * ждущий требует оператора, остановленный — нет. */
export function executionTone(execution: string): BadgeTone {
  switch (execution) {
    case 'running':
    case 'succeeded':
      return 'success';
    case 'waiting':
      return 'warning';
    case 'failed':
      return 'danger';
    default:
      return 'muted';
  }
}

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
}

/** Сводка выбранного узла: исход, актор и моменты — по последнему
 * посещению, инструкция — из графа роя. */
export function nodeView(run: PipelineRun | null, graph: RunGraph | null, id: string): NodeView {
  const visits = (run?.nodes ?? []).filter((step) => step.id === id);
  const last = visits[visits.length - 1];
  const definition = graph?.nodes?.find((node) => node.id === id);
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
  };
}

/** Колонки времени шага есть, только если сервер записал моменты: иначе
 * колонка была бы сплошным прочерком. */
export function hasStepTimes(run: PipelineRun | null): boolean {
  return (run?.nodes ?? []).some((step) => Boolean(step.startedAt || step.finishedAt));
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
