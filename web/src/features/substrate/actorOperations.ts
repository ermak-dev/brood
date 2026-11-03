import { useCallback, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useNotificationStore } from '@/stores';
import { CommandError, sessionApi } from '@/services/brood/commands';
import { actorsApi, type Actor } from '@/services/brood/substrate';
import type { ManagedActionId } from './actorActions';

/** Жизненный цикл — существующий actor API (resume/stop/reset, DELETE);
 * сессия и workspace — команды каталога. Возвращает, известен ли исход. */
export async function performManagedAction(
  swarm: string,
  name: string,
  id: ManagedActionId
): Promise<{ unconfirmed: boolean }> {
  switch (id) {
    case 'start':
      await actorsApi.act(swarm, name, 'resume');
      return { unconfirmed: false };
    case 'stop':
    case 'reset':
      await actorsApi.act(swarm, name, id);
      return { unconfirmed: false };
    case 'delete':
      await actorsApi.remove(swarm, name);
      return { unconfirmed: false };
    case 'restart':
    case 'recycle':
    case 'wipe':
      return { unconfirmed: (await sessionApi[id](swarm, name)).unconfirmed };
    default:
      throw new Error(`${id} is not a mutation`);
  }
}

/** Отказ последнего действия: держится на странице до закрытия или
 * следующего действия, а не исчезает вместе с уведомлением. */
export interface ActionFailure {
  name: string;
  /** Подпись действия для заголовка; пусто — только имя актора. */
  action?: ManagedActionId;
  error: unknown;
}

/** Неизвестный исход: держится до явного закрытия, повтор — решение человека. */
export interface UnknownOutcome {
  name: string;
  action: ManagedActionId;
  detail: string;
}

/** brood: состояние действий над managed-акторами одного роя. Блокировка
 * строки — синхронная (ref), поэтому двойной клик не уходит вторым запросом;
 * после любого исхода список перечитывается. Автоповтора нет. */
export function useActorOperations(swarm: string, refresh: () => Promise<void>) {
  const { t } = useTranslation();
  const notify = useNotificationStore((state) => state.showNotification);
  const inflight = useRef(new Set<string>());
  const [pending, setPending] = useState<Record<string, ManagedActionId>>({});
  const [failure, setFailure] = useState<ActionFailure | null>(null);
  const [unknown, setUnknown] = useState<UnknownOutcome[]>([]);

  const remember = useCallback((outcome: UnknownOutcome) => {
    setUnknown((items) => [
      outcome,
      ...items.filter((item) => item.name !== outcome.name || item.action !== outcome.action),
    ]);
  }, []);

  const perform = useCallback(
    async (actor: Actor, id: ManagedActionId) => {
      const name = actor.metadata.name;
      if (id === 'terminal' || inflight.current.has(name)) return;
      inflight.current.add(name);
      setPending((items) => ({ ...items, [name]: id }));
      setFailure(null);
      try {
        const { unconfirmed } = await performManagedAction(swarm, name, id);
        if (unconfirmed) remember({ name, action: id, detail: '' });
        else notify(t(`actors.ops.${id}.done`, { name }), 'success');
      } catch (error) {
        if (error instanceof CommandError && error.unconfirmed) {
          remember({ name, action: id, detail: error.message });
        } else {
          setFailure({ name, action: id, error });
        }
      } finally {
        inflight.current.delete(name);
        setPending((items) => {
          const next = { ...items };
          delete next[name];
          return next;
        });
        await refresh();
      }
    },
    [notify, refresh, remember, swarm, t]
  );

  const dismissUnknown = useCallback((outcome: UnknownOutcome) => {
    setUnknown((items) => items.filter((item) => item !== outcome));
  }, []);

  return {
    pending,
    failure,
    setFailure,
    clearFailure: useCallback(() => setFailure(null), []),
    unknown,
    dismissUnknown,
    perform,
  };
}
