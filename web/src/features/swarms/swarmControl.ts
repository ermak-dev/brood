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

/** Предел задачи прохода в байтах UTF-8 — тот же, что у сервера (#242). */
export const MAX_GOAL_BYTES = 4096;

/** Почему сервер отвергнет задачу прохода: ключ причины в локалях. */
export type GoalProblem = 'empty' | 'too_long' | 'invalid_utf8' | 'control' | 'envelope';

const encoder = new TextEncoder();

/** Размер задачи так, как его считает сервер: байты UTF-8, не символы. */
export function goalBytes(goal: string): number {
  return encoder.encode(goal).length;
}

// Пробелы, которые снимает strings.TrimSpace сервера (unicode.IsSpace):
// trim() JavaScript снимает U+FEFF и оставляет U+0085, и пустота разошлась бы.
const onlySpaces = /^[\t-\r \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]*$/;
// Закрывающий тег конверта сообщения гостя: с ним гость не примет ни одной
// инструкции прохода, сервер отвергает такую задачу до записи.
const envelopeClosingTag = '</cross-session-message>';

/** Управляющий символ unicode.IsControl (C0, DEL, C1), кроме перевода
 * строки и табуляции. Все они в BMP, поэтому хватает единиц UTF-16. */
function hasControl(text: string): boolean {
  for (let i = 0; i < text.length; i += 1) {
    const unit = text.charCodeAt(i);
    if ((unit < 0x20 && unit !== 0x0a && unit !== 0x09) || (unit >= 0x7f && unit <= 0x9f)) {
      return true;
    }
  }
  return false;
}

/** Одиночная суррогатная половина: такой строки нет в UTF-8, и при отправке
 * она молча стала бы U+FFFD, то есть не той задачей, что набрана. */
function hasLoneSurrogate(text: string): boolean {
  for (let i = 0; i < text.length; i += 1) {
    const unit = text.charCodeAt(i);
    if (unit >= 0xdc00 && unit <= 0xdfff) return true;
    if (unit >= 0xd800 && unit <= 0xdbff) {
      const next = text.charCodeAt(i + 1);
      if (!(next >= 0xdc00 && next <= 0xdfff)) return true;
      i += 1;
    }
  }
  return false;
}

/** Причина, по которой сервер отвергнет задачу прохода, в его порядке
 * проверок; null — задача годится. Панель проверяет заранее, чтобы отказ
 * был виден у поля без запроса; решает всё равно сервер. Задача не
 * обрезается и не меняется. */
export function goalProblem(goal: string): GoalProblem | null {
  if (onlySpaces.test(goal)) return 'empty';
  if (goalBytes(goal) > MAX_GOAL_BYTES) return 'too_long';
  if (hasLoneSurrogate(goal)) return 'invalid_utf8';
  if (hasControl(goal)) return 'control';
  if (goal.includes(envelopeClosingTag)) return 'envelope';
  return null;
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
