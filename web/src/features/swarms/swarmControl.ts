/**
 * Чистая логика управления исполнением роя (#218): какое действие доступно,
 * какое определение выбрано и какая личность у снимка. Сервер остаётся
 * судьёй каждого запуска и остановки; панель только не предлагает того,
 * что он заведомо отвергнет.
 */
import type { AccessSwarm } from '@/services/brood/access';
import type { PipelineRun, SnapshotIdentity, WorkflowDefinition } from '@/services/brood/pipelines';
import { swarmCapabilities } from '@/features/access/shardState';
import { swarmExecution } from './swarmState';

/** start — первый запуск с выбором определения, restart — новый проход на
 * закреплённом снимке, stop — остановка идущего или ждущего прохода. */
export type ExecutionAction = 'start' | 'restart' | 'stop';

const startable = new Set(['idle', 'succeeded', 'failed', 'stopped']);
const stoppable = new Set(['running', 'waiting']);

/** Единственное действие исполнения для этого пользователя. run: undefined —
 * исполнение ещё не прочитано или не читается, null — у роя нет снимка.
 * Недоступное действие не показывается вовсе: ни зритель, ни неактивный рой,
 * ни незнакомое панели состояние не получают кнопки-призрака. */
export function executionAction(
  swarm: AccessSwarm,
  administrator: boolean,
  run: PipelineRun | null | undefined
): ExecutionAction | null {
  if (run === undefined || !swarmCapabilities(swarm, administrator).manage) return null;
  const execution = swarmExecution(run);
  if (stoppable.has(execution)) return 'stop';
  if (!startable.has(execution)) return null;
  return run ? 'restart' : 'start';
}

/** Личность снимка, записанная роем; без неё — null, а не догадка по имени. */
export function snapshotIdentity(run: PipelineRun | null): SnapshotIdentity | null {
  const identity = run?.definition;
  if (
    !identity ||
    typeof identity.name !== 'string' ||
    identity.name === '' ||
    !Number.isSafeInteger(identity.version) ||
    identity.version < 1 ||
    typeof identity.revision !== 'string' ||
    identity.revision === ''
  ) {
    return null;
  }
  return { name: identity.name, version: identity.version, revision: identity.revision };
}

/** Определение повторного запуска — то, что закрепил снимок. Без записанной
 * личности — имя графа: каталог требует его совпадения с именем определения. */
export function restartDefinition(run: PipelineRun): string {
  return snapshotIdentity(run)?.name ?? run.name;
}

/** Выбранное определение — только из каталога этого проекта, в его текущей
 * версии. Из нескольких панель не выбирает за оператора; единственное
 * выбрано сразу. */
export function definitionChoice(
  definitions: WorkflowDefinition[] | null,
  chosen: string
): WorkflowDefinition | undefined {
  if (!definitions) return undefined;
  if (chosen === '') return definitions.length === 1 ? definitions[0] : undefined;
  return definitions.find((definition) => definition.name === chosen);
}
