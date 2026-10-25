import type { ReactNode } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { EmptyState } from '@/components/ui/EmptyState';
import { useProjectStore } from '@/stores';
import styles from '../substrate.module.scss';

interface ProjectScopeProps {
  title: string;
  /** The page itself, mounted only once a project is active -- so its hooks
   * always run with one. */
  children: (project: string) => ReactNode;
}

/** brood: project-scoped pages (actors, templates) need the active project
 * from the header switcher; without one, say where to pick it. */
export function ProjectScope({ title, children }: ProjectScopeProps) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const project = useProjectStore((state) => state.project);
  if (project) return <>{children(project)}</>;
  return (
    <div className={styles.page}>
      <header className={styles.header}>
        <div className={styles.copy}>
          <h1 className={styles.title}>{title}</h1>
        </div>
      </header>
      <div className={styles.empty}>
        <EmptyState
          title={t('substrate.no_project')}
          description={t('substrate.no_project_hint')}
          action={<Button onClick={() => navigate('/projects')}>{t('substrate.no_project_action')}</Button>}
        />
      </div>
    </div>
  );
}
