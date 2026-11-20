import { useTranslation } from 'react-i18next';
import { useAdministrator } from './hooks';
import type { GuideStep } from './table/TablePage';

type Step = 'pool' | 'project' | 'swarm' | 'agent';

/** The way from an empty installation to a working agent, in order, with
 * the step the empty page is waiting for marked. Only an administrator can
 * do the first step, so only an administrator gets its link. */
export function useGuide(current: Step): GuideStep[] {
  const { t } = useTranslation();
  const administrator = useAdministrator();
  const step = (id: Step, to?: string): GuideStep => ({
    id,
    label: t(`brood.guide.${id === 'pool' && !administrator ? 'pool_member' : id}`),
    to,
    current: id === current,
  });
  return [
    step('pool', administrator ? '/pools' : undefined),
    step('project', '/projects'),
    step('swarm', '/swarms'),
    step('agent', '/agents'),
  ];
}
