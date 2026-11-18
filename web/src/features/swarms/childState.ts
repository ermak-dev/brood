import type { ChildGroup } from '@/services/brood/children';

/** Только полные сведения о заведомо незапущенной неполной группе дают форму abort.
 * Это подсказка интерфейса: текущую сессию, права и revision проверяет сервер. */
export function canAbortChildGroup(group: ChildGroup, owner: boolean): boolean {
  return (
    owner &&
    group.state === 'open' &&
    Number.isSafeInteger(group.revision) &&
    group.revision > 0 &&
    group.revision < Number.MAX_SAFE_INTEGER &&
    group.registered === group.children.length &&
    group.registered < group.total &&
    group.started === 0 &&
    group.children.every(
      (task) =>
        task.state === 'cancelled' ||
        (task.state === 'pending' && ['', 'queued', 'not_sent'].includes(task.delivery))
    )
  );
}
