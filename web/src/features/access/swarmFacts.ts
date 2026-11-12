/**
 * Определение и числа роя в строке списка (#268): строка
 * `GET /api/projects/{id}/swarms` несёт личность определения снимка
 * (`definition`), число участников (`memberCount`) и managed-акторов
 * (`managedActorCount`) одной выборкой на весь список, как сводку
 * исполнения (#234). Панель роёв ради них не опрашивает.
 *
 * Managed-акторы -- не все акторы роя: native-акторов Substrate строка не
 * считает, поэтому число так и называется, а страница роя по-прежнему
 * читает своих акторов сама.
 */
import type { AccessSwarm } from '@/services/brood/access';
import type { SnapshotIdentity } from '@/services/brood/pipelines';
import { snapshotIdentity } from '@/features/swarms/swarmControl';
import { isCount } from './swarmActivity';

export interface SwarmFacts {
  /** Определение, из которого снят снимок роя; у не запущенного роя его нет. */
  definition?: SnapshotIdentity;
  members?: number;
  managedActors?: number;
}

/** Сведения из строки; поля нет (старый сервер) или в нём брак -- нет и
 * сведения, а не ноль. Брак одного поля остальных не прячет. */
export function swarmFactsOf(swarm: AccessSwarm): SwarmFacts {
  const facts: SwarmFacts = {};
  const definition = snapshotIdentity(swarm);
  if (definition) facts.definition = definition;
  if (isCount(swarm.memberCount)) facts.members = swarm.memberCount;
  if (isCount(swarm.managedActorCount)) facts.managedActors = swarm.managedActorCount;
  return facts;
}

export type SwarmFactColumns = Record<keyof SwarmFacts, boolean>;

/** Какие сведения показывает список: те, что несёт хоть одна строка. Строки
 * старого сервера новых колонок не дают -- таблица остаётся прежней. */
export function factColumns(rows: AccessSwarm[]): SwarmFactColumns {
  const facts = rows.map(swarmFactsOf);
  const carried = (key: keyof SwarmFacts) => facts.some((row) => row[key] !== undefined);
  return {
    definition: carried('definition'),
    members: carried('members'),
    managedActors: carried('managedActors'),
  };
}
