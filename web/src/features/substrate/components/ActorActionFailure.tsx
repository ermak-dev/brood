import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import { Button } from '@/components/ui/Button';
import { errorMessage } from '@/services/brood/http';
import { DASH, formatCpu, formatMemory, selectorText } from '../format';
import { placementFailure, type PlacementReason } from '../placementFailure';
import { StateBadge } from './StateBadge';
import styles from '../substrate.module.scss';

const REASONS: Record<string, string> = {
  'incomplete registration': 'registration',
  'worker is not active': 'inactive',
  'snapshot sandbox class mismatch': 'sandbox',
  'worker is not on a required node': 'node',
  'template selector mismatch': 'template',
  'actor selector mismatch': 'actor',
  'insufficient actor slots': 'slots',
  'insufficient CPU': 'cpu',
  'insufficient memory bytes': 'memory',
};

function ResourceReason({ reason }: { reason: PlacementReason }) {
  const { t } = useTranslation();
  let format: (value: string | undefined) => string = (value) => value ?? DASH;
  if (reason.code === 'insufficient CPU') format = formatCpu;
  if (reason.code === 'insufficient memory bytes') format = formatMemory;
  return <>{t('placement.resources', {
    required: format(reason.required), available: format(reason.available),
  })}</>;
}

export function ActorActionFailure({ name, action, error, administrator, onClose }: {
  name: string;
  /** Подпись отклонённого действия: отказ называет, что именно не удалось. */
  action?: string;
  error: unknown;
  administrator: boolean;
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const failure = placementFailure(error);
  const diagnostic = administrator ? failure?.diagnostic : null;
  return (
    <section role="alert" className={`${styles.notice} ${styles.operationFailure}`}>
      <strong>
        {failure
          ? t('placement.title', { name })
          : action
            ? t('actors.ops.refused', { action, name })
            : name}
      </strong>
      {!failure ? <p>{errorMessage(error)}</p> : diagnostic ? (
        <>
          <dl>
            <dt>{t('placement.template_selector')}</dt>
            <dd><code>{selectorText({ matchLabels: diagnostic.templateSelector })}</code></dd>
            <dt>{t('placement.actor_selector')}</dt>
            <dd><code>{selectorText({ matchLabels: diagnostic.actorSelector })}</code></dd>
          </dl>
          {!diagnostic.workers.length && <p>{t('placement.empty')}</p>}
          {diagnostic.workers.map((worker) => (
            <div key={worker.name}>
              <strong>{t('placement.worker', { name: worker.name })}</strong>
              <ul>
                {!worker.reasons.length && <li>{t('placement.unknown')}</li>}
                {worker.reasons.map((reason, i) => (
                  <li key={i}>
                    {t(`placement.reasons.${REASONS[reason.code] ?? 'unknown'}`)}{' '}
                    {reason.state && <StateBadge state={reason.state.replace('WORKER_STATE_', '').toLowerCase()} namespace="workers.state" />}
                    {reason.required !== undefined && <ResourceReason reason={reason} />}
                  </li>
                ))}
              </ul>
            </div>
          ))}
          <p>{t('placement.advisory')}</p>
        </>
      ) : <p>{t(administrator ? 'placement.unavailable' : 'placement.member_hint')}</p>}
      {failure && administrator && (
        <p><Link to="/workers">{t('nav.workers')}</Link>{' · '}
          <Link to="/worker-pools">{t('nav.worker_pools')}</Link></p>
      )}
      <Button variant="ghost" size="sm" onClick={onClose}>{t('common.close')}</Button>
    </section>
  );
}
