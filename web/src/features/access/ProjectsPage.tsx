import { useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { accessApi, type AccessProject } from '@/services/brood/access';
import { EntityTable, type Column } from '@/features/substrate/components/EntityTable';
import { PageHeader } from '@/features/substrate/components/PageHeader';
import {
  AccessEditor,
  LoadState,
  useAccessActions,
  useAccessResource,
  useAdministrator,
} from './shared';
import { projectSectionPath } from './state';
import styles from '@/features/substrate/substrate.module.scss';

export function AccessProjectsPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const admin = useAdministrator();
  const resource = useAccessResource(accessApi.projects);
  const { run, confirm, busy } = useAccessActions(resource.refresh);
  const [creating, setCreating] = useState(false);
  const columns: Column<AccessProject>[] = [
    { key: 'name', label: t('substrate.name'), primary: true, render: (project) => project.name },
    { key: 'id', label: t('access.id'), mono: true, render: (project) => project.id },
  ];
  return (
    <div className={styles.page}>
      <PageHeader
        title={t('access.projects')}
        actions={
          admin && <Button onClick={() => setCreating(true)}>{t('access.create_project')}</Button>
        }
      />
      <p className={styles.sectionHint}>
        {t(admin ? 'access.admin_policy' : 'access.member_policy')}
      </p>
      <LoadState {...resource} retry={resource.refresh}>
        <EntityTable
          columns={columns}
          rows={resource.data ?? []}
          rowKey={(project) => project.id}
          // Строка ведёт в проект; его разделы -- рои и участники -- живут
          // внутри него, а не отдельными пунктами меню.
          onRowClick={(project) => navigate(projectSectionPath('swarms', project.id))}
          actionsLabel={t('substrate.actions')}
          // Участнику действий над проектом не положено: без админа столбец
          // действий не рисуется вовсе, а не стоит пустым.
          actions={
            admin
              ? (project) => (
                  <Button
                    variant="ghost"
                    size="sm"
                    disabled={busy}
                    className={styles.dangerText}
                    onClick={() => confirm(project.name, () => accessApi.deleteProject(project.id))}
                  >
                    {t('substrate.delete')}
                  </Button>
                )
              : undefined
          }
        />
        {resource.data?.length === 0 && <p>{t('access.no_projects')}</p>}
      </LoadState>
      {creating && admin && (
        <AccessEditor
          title={t('access.create_project')}
          fields={[{ name: 'name', label: t('substrate.name'), required: true }]}
          close={() => setCreating(false)}
          submit={(values) => run(() => accessApi.createProject(values.name.trim()))}
        />
      )}
    </div>
  );
}
