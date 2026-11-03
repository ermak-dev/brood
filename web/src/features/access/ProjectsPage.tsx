import { useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { accessApi, type AccessProject } from '@/services/brood/access';
import { EntityPanel } from '@/features/substrate/components/EntityPanel';
import { EntityTable, type Column } from '@/features/substrate/components/EntityTable';
import { PageHeader } from '@/features/substrate/components/PageHeader';
import { panelGroups, projectPlan, type ActionSpec, type ProjectAction } from './panelActions';
import {
  AccessEditor,
  LoadState,
  PanelDetails,
  StatusBadge,
  useAccessActions,
  useAccessResource,
  useAdministrator,
} from './shared';
import { retainedRow } from './state';
import { useProjectStore } from '@/stores';
import styles from '@/features/substrate/substrate.module.scss';

const sameProject = (row: AccessProject, open: AccessProject) => row.id === open.id;

/** Строка проекта открывает его панель (#157): выбор проекта -- главное
 * действие в ней, а не щелчок по строке. */
export function AccessProjectsPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const admin = useAdministrator();
  const selected = useProjectStore((state) => state.project);
  const setProject = useProjectStore((state) => state.setProject);
  const choose = (project: AccessProject) => {
    setProject(project.id);
    navigate('/swarms');
  };
  const resource = useAccessResource(accessApi.projects);
  const { run, confirm, busy } = useAccessActions(resource.refresh);
  const [creating, setCreating] = useState(false);
  const [opened, setOpened] = useState<AccessProject | null>(null);
  const viewing = retainedRow(opened, resource, sameProject);
  const columns: Column<AccessProject>[] = [
    { key: 'name', label: t('substrate.name'), primary: true, render: (project) => project.name },
    { key: 'id', label: t('access.id'), mono: true, render: (project) => project.id },
  ];
  const actions = (project: AccessProject): Record<ProjectAction, ActionSpec> => ({
    select: {
      label: t('access.choose_project'),
      description: t('access.panel.select_hint'),
      onSelect: () => {
        setOpened(null);
        choose(project);
      },
    },
    details: {
      label: t('access.project_details'),
      description: t('access.panel.details_hint'),
      onSelect: () => {
        setOpened(null);
        setProject(project.id);
        navigate(`/projects/${encodeURIComponent(project.id)}`);
      },
    },
    delete: {
      label: t('substrate.delete'),
      description: t('access.panel.delete_project_hint'),
      pending: busy,
      onSelect: () =>
        confirm(
          project.name,
          async () => {
            await accessApi.deleteProject(project.id);
            setOpened(null);
            if (selected === project.id) setProject('');
          },
          t('substrate.delete'),
          t('access.confirm_project', { name: project.name })
        ),
    },
  });
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
          onRowClick={setOpened}
          isActive={(project) => project.id === selected}
        />
        {resource.data?.length === 0 && <p>{t('access.no_projects')}</p>}
      </LoadState>
      <EntityPanel
        open={viewing !== null}
        onClose={() => setOpened(null)}
        title={viewing?.name ?? ''}
        status={
          viewing?.id === selected && (
            <StatusBadge tone="success">{t('substrate.switcher_title')}</StatusBadge>
          )
        }
        actions={viewing ? panelGroups(projectPlan(admin), actions(viewing), t) : []}
      >
        {viewing && (
          <PanelDetails details={[{ label: t('access.id'), value: viewing.id, mono: true }]} />
        )}
      </EntityPanel>
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
