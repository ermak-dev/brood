import type { GateKind, HumanGate, PipelineRun, RunStatus } from '@/services/brood/pipelines';

/** Живые проходы сверху; сортировка стабильна внутри одного статуса. */
const STATUS_ORDER: Record<RunStatus, number> = {
  running: 0,
  waiting_human: 1,
  failed: 2,
  stopped: 3,
  succeeded: 4,
  idle: 5,
};

/** Остановленный и ещё не начатый проход — не сбой: приглушены. */
export function runTone(status: RunStatus): 'success' | 'warning' | 'danger' | 'muted' {
  if (status === 'succeeded' || status === 'running') return 'success';
  if (status === 'waiting_human') return 'warning';
  if (status === 'stopped' || status === 'idle') return 'muted';
  return 'danger';
}

/** Живой вопрос проходит вперёд завершённых и проваленных. */
export function sortRuns(runs: PipelineRun[]): PipelineRun[] {
  return [...runs].sort((left, right) => {
    const order = STATUS_ORDER[left.status] - STATUS_ORDER[right.status];
    if (order !== 0) return order;
    const a = left.startedAt ?? '';
    const b = right.startedAt ?? '';
    return a < b ? 1 : a > b ? -1 : 0;
  });
}

export function gateVisitKey(gate: HumanGate): string {
  return JSON.stringify([gate.runId, gate.nodeId, gate.visitSequence]);
}

export interface GateState {
  choice: string | null;
  freeform: string;
  confirming: boolean;
}

export function initialGateState(): GateState {
  return { choice: null, freeform: '', confirming: false };
}

/** Выбор варианта сбрасывает начатое подтверждение: вопрос снова другой. */
export function selectOption(state: GateState, optionId: string): GateState {
  return { ...state, choice: optionId, confirming: false };
}

/** Точка входа ответа: id варианта для yes_no/multiple_choice/confirmation,
 * свободный текст для freeform. */
export function gateAnswer(state: GateState): string {
  return state.freeform.trim();
}

export function gateReady(gate: HumanGate, state: GateState): boolean {
  return gate.kind === 'freeform'
    ? gateAnswer(state) !== ''
    : gate.options.some((option) => option.id === state.choice);
}

export type GateDecision = { act: 'none' } | { act: 'confirm' } | { act: 'send'; answer: string };

/** Что делает нажатие «отправить»: подтверждение спрашивает второй клик,
 * неполный ответ не уходит вовсе. */
export function gateDecision(gate: HumanGate, state: GateState): GateDecision {
  if (!gateReady(gate, state)) return { act: 'none' };
  if (gate.kind === 'confirmation' && !state.confirming) return { act: 'confirm' };
  return {
    act: 'send',
    answer: gate.kind === 'freeform' ? gateAnswer(state) : (state.choice as string),
  };
}

export function gateKindAllowsOptions(kind: GateKind): boolean {
  return kind !== 'freeform';
}

/** Вопрос и номер посещения должны прийти от сервера, а не из подписи узла. */
export function openGate(run: PipelineRun): HumanGate | null {
  const gate = run.gate;
  if (
    (run.status !== 'waiting_human' && run.status !== 'running') ||
    !gate ||
    gate.runId !== run.id ||
    !Number.isSafeInteger(gate.visitSequence) ||
    gate.visitSequence < 1 ||
    !run.nodes.some((node) => node.id === gate.nodeId && node.kind === 'human' && node.status === 'active')
  ) return null;
  return gate;
}

/** Завершённый проход дальше не опрашивать. */
export function runIsLive(run: PipelineRun | null): boolean {
  return run === null || run.status === 'running' || run.status === 'waiting_human';
}
