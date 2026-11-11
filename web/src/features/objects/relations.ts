import type { SwarmMember } from '@/services/brood/access';
import type { Actor, ActorTemplate, Worker } from '@/services/brood/substrate';
import type { ObjectRef } from './address';

/** Связи объектов (#195): кто на кого ссылается, по данным, которые сервер
 * уже отдаёт. Связанный объект -- имя и адрес, а адрес -- только когда
 * страница его откроет: шаблон есть в списке роя, воркер и пул открыты
 * администратору (их страницы только у него), панель актора пользователю
 * доступна. Иначе имя остаётся текстом, а не ведёт к отказу. */

/** Проект и рой, в которых живут актор и шаблон: имена повторяются в разных
 * роях, адрес без роя их не открывает. */
export interface SwarmScope {
  project: string;
  swarm: string;
}

/** Связанный объект: имя и, если его страница откроется, адрес. */
export interface Related {
  name: string;
  ref?: ObjectRef;
}

function related(name: string, ref: ObjectRef | false): Related {
  return ref ? { name, ref } : { name };
}

const byName = (a: string, b: string) => (a < b ? -1 : a > b ? 1 : 0);

/** Шаблон актора: native -- его actorTemplate, managed -- виртуальный шаблон
 * пула Proxmox, который сервер отдаёт тем же полем. Ссылка -- только на
 * шаблон из списка роя: удалённый или выключенный шаблон панель не откроет. */
export function actorTemplate(
  actor: Actor,
  scope: SwarmScope,
  templates: ActorTemplate[] | null
): Related | undefined {
  const name = actor.actorTemplate?.name;
  if (!name) return undefined;
  const listed = templates?.some((template) => template.metadata.name === name) ?? false;
  return related(name, listed && { kind: 'template', ...scope, name });
}

/** Воркер native-актора -- status.workerAssignment.worker: назначение есть,
 * пока актор на воркере (не suspended и не crashed). У managed-актора
 * native-воркера нет, он в CT Proxmox. */
export function actorWorker(actor: Actor, administrator: boolean): Related | undefined {
  if (actor.provider === 'proxmox') return undefined;
  const name = actor.status?.workerAssignment?.worker?.name;
  if (!name) return undefined;
  return related(name, administrator && { kind: 'worker', name });
}

type PoolRef = Extract<ObjectRef, { kind: 'pool' | 'proxmoxPool' }>;

/** Пул актора: native -- WorkerPool его назначения, managed -- пул Proxmox
 * из placement. */
function poolOf(actor: Actor): PoolRef | undefined {
  const managed = actor.provider === 'proxmox';
  const name = managed ? actor.placement?.pool : actor.status?.workerAssignment?.workerPool;
  if (!name) return undefined;
  return { kind: managed ? 'proxmoxPool' : 'pool', name };
}

/** Пул, где работает актор. */
export function actorPool(actor: Actor, administrator: boolean): Related | undefined {
  const pool = poolOf(actor);
  return pool && related(pool.name, administrator && pool);
}

/** Актор как связанный объект: ссылка на его панель в его рое, если
 * пользователь может её открыть (canViewActorDetails, не golden). */
export function actorRelated(actor: Actor, scope: SwarmScope, openable: boolean): Related {
  const name = actor.metadata.name;
  return related(name, openable && { kind: 'actor', ...scope, name });
}

/** Акторы, созданные из шаблона, по имени. Golden-актор шаблона (ateapi
 * называет его uid шаблона) -- служебный, его среди них нет. */
export function templateActors(template: ActorTemplate, actors: Actor[]): Actor[] {
  const { name, uid } = template.metadata;
  return actors
    .filter((actor) => actor.actorTemplate?.name === name && actor.metadata.name !== uid)
    .sort((a, b) => byName(a.metadata.name, b.metadata.name));
}

/** Пулы, где работают акторы шаблона, каждый один раз, по имени. Своего пула
 * у native-шаблона нет: селектор подходит многим, а managed-шаблон своего
 * пула Proxmox полем не называет, поэтому связь -- через акторов. */
export function templatePools(
  template: ActorTemplate,
  actors: Actor[],
  administrator: boolean
): Related[] {
  const pools = new Map<string, PoolRef>();
  for (const actor of templateActors(template, actors)) {
    const pool = poolOf(actor);
    if (pool) pools.set(`${pool.kind}/${pool.name}`, pool);
  }
  return [...pools.values()]
    .sort((a, b) => byName(a.name, b.name))
    .map((pool) => related(pool.name, administrator && pool));
}

/** Пул воркера; страница воркеров -- только у администратора, ссылка всегда. */
export function workerPool(worker: Worker): Related | undefined {
  const name = worker.workerPool;
  return name ? { name, ref: { kind: 'pool', name } } : undefined;
}

/** Воркеры пула по имени. */
export function poolWorkers(pool: string, workers: Worker[]): Worker[] {
  return workers
    .filter((worker) => worker.workerPool === pool)
    .sort((a, b) => byName(a.metadata.name, b.metadata.name));
}

export interface SwarmCounts {
  actors: number | null;
  templates: number | null;
  members: number | null;
}

/** Сколько объектов у роя: столько строк откроет ссылка (golden-актор --
 * тоже строка списка). Непрочитанный список -- неизвестно, не ноль. */
export function swarmCounts(lists: {
  actors: Actor[] | null;
  templates: ActorTemplate[] | null;
  members: SwarmMember[] | null;
}): SwarmCounts {
  return {
    actors: lists.actors?.length ?? null,
    templates: lists.templates?.length ?? null,
    members: lists.members?.length ?? null,
  };
}
