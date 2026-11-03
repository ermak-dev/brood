import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { actorsApi, type Actor, type ActorAction } from '@/services/brood/substrate';
import { actorActions } from '../actorLifecycle';

/** Действия native-актора: терминал и suspend/resume. Managed-строки
 * используют ManagedActorActions с главным действием по состоянию и меню. */
export function ActorLifecycleActions({
  actor,
  swarm,
  disabled,
  onAction,
}: {
  actor: Actor;
  swarm: string;
  disabled: boolean;
  onAction: (action: ActorAction) => void;
}) {
  const { t } = useTranslation();
  if (actor.provider === 'proxmox') return null;
  return (
    <>
      <a href={actorsApi.terminal(swarm, actor.metadata.name)} target="_blank" rel="noopener noreferrer">
        {t('access.terminal')}
      </a>
      {actorActions(actor).map((action) => (
        <Button
          key={action}
          variant="ghost"
          size="sm"
          disabled={disabled}
          onClick={() => onAction(action)}
        >
          {t(`actors.${action}`)}
        </Button>
      ))}
    </>
  );
}
