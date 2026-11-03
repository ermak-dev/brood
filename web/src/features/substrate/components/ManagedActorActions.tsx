import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { actorsApi, type Actor } from '@/services/brood/substrate';
import {
  blockedText,
  managedActionPlan,
  type ActionState,
  type ManagedActionId,
} from '../actorActions';
import { ActionMenu } from './ActionMenu';
import { useHint } from './useHint';
import styles from '../substrate.module.scss';

function ActionButton({
  actor,
  state,
  variant,
  onAction,
}: {
  actor: Actor;
  state: ActionState;
  variant: 'primary' | 'secondary';
  onAction: (id: ManagedActionId) => void;
}) {
  const { t } = useTranslation();
  const hint = useHint(blockedText(actor, state, t) ?? t(`actors.ops.${state.id}.hint`));
  return (
    <>
      <Button
        type="button"
        variant={variant}
        size="sm"
        aria-disabled={state.enabled ? undefined : true}
        className={state.enabled ? undefined : styles.actionBlocked}
        onClick={state.enabled ? () => onAction(state.id) : undefined}
        {...hint.trigger}
      >
        {t(`actors.ops.${state.id}.label`)}
      </Button>
      {hint.node}
    </>
  );
}

function TerminalLink({ actor, swarm }: { actor: Actor; swarm: string }) {
  const { t } = useTranslation();
  const hint = useHint(t('actors.ops.terminal.hint'));
  return (
    <>
      <a
        className="btn btn-primary btn-sm"
        href={actorsApi.terminal(swarm, actor.metadata.name)}
        target="_blank"
        rel="noopener noreferrer"
        {...hint.trigger}
      >
        {t('actors.ops.terminal.label')}
      </a>
      {hint.node}
    </>
  );
}

/** brood: действия managed-актора. Главное — по состоянию (терминал у
 * работающего, Start у остановленного), Stop — рядом второстепенным. Реже
 * нужные — в меню группами: сессия, рабочая копия, затем отдельно
 * разрушительные. Пока действие строки идёт, вместо кнопок — его статус.
 * Без canManage (viewer) — только детали: данные без изменяющих действий. */
export function ManagedActorActions({
  actor,
  swarm,
  canManage,
  pending,
  onAction,
  onDetails,
}: {
  actor: Actor;
  swarm: string;
  canManage: boolean;
  pending?: ManagedActionId;
  onAction: (id: ManagedActionId) => void;
  /** Нет — кнопка деталей не нужна (действия уже в деталях). */
  onDetails?: () => void;
}) {
  const { t } = useTranslation();
  const name = actor.metadata.name;
  const plan = managedActionPlan(actor, pending);
  const groups = plan.groups.map((group) => ({
    id: group.id,
    label: t(`actors.ops.groups.${group.id}`),
    actions: group.actions.map((state) => ({
      id: state.id,
      label: t(`actors.ops.${state.id}.label`),
      description: t(`actors.ops.${state.id}.hint`),
      blocked: blockedText(actor, state, t),
      danger: group.id === 'danger',
      onSelect: () => onAction(state.id),
    })),
  }));

  return (
    <>
      {canManage && (
        <span className={styles.actorActionGroup}>
          {pending ? (
            <span className={styles.pendingNote} role="status">
              <span className="loading-spinner" aria-hidden="true" />
              {t(`actors.ops.${pending}.pending`, { name })}
            </span>
          ) : (
            <>
              {plan.primary.id === 'terminal' && plan.primary.enabled ? (
                <TerminalLink actor={actor} swarm={swarm} />
              ) : (
                <ActionButton actor={actor} state={plan.primary} variant="primary" onAction={onAction} />
              )}
              <ActionButton actor={actor} state={plan.stop} variant="secondary" onAction={onAction} />
            </>
          )}
        </span>
      )}
      <span className={styles.actorActionGroup}>
        {onDetails && (
          <Button type="button" variant="ghost" size="sm" onClick={onDetails}>
            {t('actors.details')}
          </Button>
        )}
        {canManage && (
          <ActionMenu
            label={t('actors.ops.more')}
            accessibleLabel={t('actors.ops.more_label', { name })}
            groups={groups}
            disabled={Boolean(pending)}
          />
        )}
      </span>
    </>
  );
}
