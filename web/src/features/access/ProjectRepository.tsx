import { useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import { accessApi, type AccessProject } from '@/services/brood/access';
import styles from '@/features/substrate/substrate.module.scss';
import { LoadState, useAccessResource } from './shared';

export function ProjectRepository({ project }: { project: AccessProject }) {
  const { t } = useTranslation();
  const load = useCallback(() => accessApi.repository(project.id), [project.id]);
  const repository = useAccessResource(load);
  const current = repository.data ?? '';

  return (
    <section className={styles.page}>
      <h2>{t('access.repository.title')}</h2>
      <p className={styles.sectionHint}>{t('access.repository.declaration_hint')}</p>
      <LoadState {...repository} retry={repository.refresh}>
        <dl className={styles.details}>
          <dt>{t('access.repository.current')}</dt>
          <dd className={current ? styles.mono : undefined}>
            {current || t('access.repository.not_set')}
          </dd>
        </dl>
      </LoadState>
    </section>
  );
}
