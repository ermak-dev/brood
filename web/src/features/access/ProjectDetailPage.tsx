import { useCallback, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { IconChevronRight } from '@/components/ui/icons';
import {
  accessApi,
  type AccessProject,
  type ProjectDetail,
  type ShardCapacity,
} from '@/services/brood/access';
import { ProjectScope } from '@/features/substrate/components/ProjectScope';
import { PageHeader } from '@/features/substrate/components/PageHeader';
import { EntityTable } from '@/features/substrate/components/EntityTable';
import { CapacityEditor } from './CapacityEditor';
import { ProjectMembers } from './PermissionsPage';
import { ProjectRepository } from './ProjectRepository';
import { LoadState, StatusBadge, useAccessActions, useAccessResource, useAdministrator } from './shared';
import styles from '@/features/substrate/substrate.module.scss';

export function ProjectDetailPage() {
  const { t } = useTranslation();
  return (
    <ProjectScope title={t('access.project_details')}>
      {(project) => <ProjectDetailView project={project} />}
    </ProjectScope>
  );
}

/** Ёмкость и резервирование шарда (#255): без инвентаря раздел свёрнут --
 * четыре «неизвестно» ничего не говорят, причина названа в заголовке. */
export function CapacitySection({ detail }: { detail: ProjectDetail }) {
  const { t } = useTranslation();
  const available = detail.usage_available;
  const rows = [
    {
      key: 'swarms',
      label: t('access.swarms'),
      used: detail.usage?.swarms,
      limit: detail.capacity.max_swarms,
    },
    {
      key: 'actors',
      label: t('actors.title'),
      used: detail.usage?.actors,
      limit: detail.capacity.max_actors,
    },
    {
      key: 'cpu',
      label: t('access.max_cpu_millicores'),
      used: detail.usage?.cpu_millicores,
      limit: detail.capacity.max_cpu_millicores,
    },
    {
      key: 'memory',
      label: t('access.max_memory_bytes'),
      used: detail.usage?.memory_bytes,
      limit: detail.capacity.max_memory_bytes,
    },
  ];
  return (
    <details className={styles.group} open={available}>
      <summary className={styles.groupSummary}>
        <IconChevronRight size={16} className={styles.groupChevron} aria-hidden="true" />
        <h2 className={styles.groupTitle}>{t('access.capacity')}</h2>
        {!available && (
          <span role="status" className={styles.groupStatus}>
            <StatusBadge tone="neutral">{t('access.unknown')}</StatusBadge>
            {t('access.usage_unknown')}
            {detail.usage_error ? `: ${detail.usage_error}` : ''}
          </span>
        )}
      </summary>
      <div className={styles.groupBody}>
        <p className={styles.sectionHint}>{t('access.usage_hint')}</p>
        <EntityTable
          rows={rows}
          rowKey={(row) => row.key}
          columns={[
            {
              key: 'resource',
              label: t('access.resource'),
              primary: true,
              render: (row) => row.label,
            },
            {
              key: 'used',
              label: t('access.used'),
              mono: available,
              render: (row) =>
                available && row.used !== undefined ? (
                  row.used
                ) : (
                  <span className={styles.muted}>{t('access.unknown')}</span>
                ),
            },
            {
              key: 'limit',
              label: t('access.limit'),
              mono: true,
              render: (row) => row.limit ?? t('access.unlimited'),
            },
          ]}
        />
        <h3>{t('access.worker_selector')}</h3>
        {Object.keys(detail.capacity.worker_selector).length === 0 ? (
          <p>{t('access.no_selector')}</p>
        ) : (
          <dl className={styles.details}>
            {Object.entries(detail.capacity.worker_selector).map(([key, value]) => (
              <div key={key}>
                <dt>{key}</dt>
                <dd>{value || '""'}</dd>
              </div>
            ))}
          </dl>
        )}
      </div>
    </details>
  );
}

function ProjectDetailView({ project }: { project: AccessProject }) {
  const { t } = useTranslation();
  const admin = useAdministrator();
  const load = useCallback(() => accessApi.project(project.id), [project.id]);
  const resource = useAccessResource(load);
  const { run } = useAccessActions(resource.refresh);
  const [editing, setEditing] = useState<ShardCapacity | null>(null);
  const detail = resource.data;
  return (
    <div className={styles.page}>
      <PageHeader
        title={project.name}
        meta={[project.id]}
        actions={
          admin &&
          detail && (
            <Button onClick={() => setEditing(detail.capacity)}>{t('access.edit_capacity')}</Button>
          )
        }
      />
      <LoadState {...resource} retry={resource.refresh}>
        {detail && <CapacitySection detail={detail} />}
      </LoadState>
      <ProjectRepository project={project} />
      <ProjectMembers project={project} />
      {editing && admin && (
        <CapacityEditor
          capacity={editing}
          close={() => setEditing(null)}
          submit={(capacity) => run(() => accessApi.setCapacity(project.id, capacity))}
        />
      )}
    </div>
  );
}
