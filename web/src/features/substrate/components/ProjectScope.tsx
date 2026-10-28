import { Fragment, type ReactNode } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { EmptyState } from '@/components/ui/EmptyState';
import { useProjectStore } from '@/stores';
import { accessApi, type AccessProject } from '@/services/brood/access';
import { LoadState, useAccessResource } from '@/features/access/shared';
import { PageHeader } from './PageHeader';
import styles from '../substrate.module.scss';

interface ProjectScopeProps {
  title: string;
  headingLevel?: 1 | 2;
  children: (project: AccessProject) => ReactNode;
}

/** Проект проверяется по доступному каталогу. Сохранённый ID не даёт прав
 * и не подменяется первым проектом, если членство было отозвано. */
export function ProjectScope({ title, children, headingLevel = 1 }: ProjectScopeProps) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const selected = useProjectStore((state) => state.project);
  const resource = useAccessResource(accessApi.projects, { headerRefresh: false });
  const project = resource.data?.find((item) => item.id === selected);
  if (project) return <Fragment key={project.id}>{children(project)}</Fragment>;
  return (
    <div className={styles.page}>
      <PageHeader title={title} headingLevel={headingLevel} />
      <LoadState {...resource} retry={resource.refresh}>
        <EmptyState
          title={t('access.select_project')}
          description={t('access.select_project_hint')}
          action={<Button onClick={() => navigate('/projects')}>{t('access.projects')}</Button>}
        />
      </LoadState>
    </div>
  );
}
