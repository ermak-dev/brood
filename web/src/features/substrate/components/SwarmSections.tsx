import { useCallback, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom';
import { accessApi, type AccessProject, type AccessSwarm } from '@/services/brood/access';
import { Button } from '@/components/ui/Button';
import { EmptyState } from '@/components/ui/EmptyState';
import { LoadState } from '@/features/access/shared';
import { useResource } from '../hooks';
import { PageHeader } from './PageHeader';
import styles from '../substrate.module.scss';

/** Каталог уже отфильтрован сервером: администратору все рои проекта,
 * участнику только собственные. Каждый редактор получает ID своего роя,
 * даже когда в общей странице встречаются одинаковые имена акторов. */
export function SwarmSections({
  project,
  title,
  children,
}: {
  project: AccessProject;
  title: string;
  children: (swarm: AccessSwarm) => ReactNode;
}) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const load = useCallback(() => accessApi.swarms(project.id), [project.id]);
  const resource = useResource(load);
  const swarms = resource.data?.filter((swarm) => swarm.state === 'active') ?? [];
  return (
    <div className={styles.page}>
      <PageHeader title={title} meta={[project.name]} />
      <LoadState {...resource} error={resource.error ?? ''} retry={resource.refresh}>
        {swarms.length ? (
          swarms.map((swarm) => <section key={swarm.id}>{children(swarm)}</section>)
        ) : (
          <EmptyState
            title={t('access.no_active_swarms')}
            action={<Button onClick={() => navigate('/swarms')}>{t('access.swarms')}</Button>}
          />
        )}
      </LoadState>
    </div>
  );
}
