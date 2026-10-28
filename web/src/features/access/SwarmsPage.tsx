import { useCallback, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { useHeaderRefresh } from '@/hooks/useHeaderRefresh';
import { accessApi, type AccessProject, type AccessSwarm } from '@/services/brood/access';
import { EntityTable, type Column } from '@/features/substrate/components/EntityTable';
import { PageHeader } from '@/features/substrate/components/PageHeader';
import {
  AccessEditor,
  LoadState,
  useAccessActions,
  useAccessResource,
  useAdministrator,
} from './shared';
import { ownerCandidates } from './state';
import { ProjectScope } from '@/features/substrate/components/ProjectScope';
import styles from '@/features/substrate/substrate.module.scss';

/** Рои глобально выбранного проекта; каталог уже отфильтрован сервером. */
export function SwarmsPage() {
  const { t } = useTranslation();
  return (
    <ProjectScope title={t('access.swarms')}>
      {(project) => <Swarms project={project} />}
    </ProjectScope>
  );
}

function Swarms({ project }: { project: AccessProject }) {
  const { t } = useTranslation();
  const admin = useAdministrator();
  const projectId = project.id;
  const load = useCallback(() => accessApi.swarms(projectId), [projectId]);
  const resource = useAccessResource(load, { headerRefresh: false });
  const { run, confirm, busy } = useAccessActions(resource.refresh);
  const [creating, setCreating] = useState(false);
  const [transfer, setTransfer] = useState<AccessSwarm | null>(null);
  // Новый владелец выбирается из участников проекта: сервер всё равно
  // откажет остальным, и угадывать username незачем. Список открыт
  // администратору -- только он и передаёт владение.
  const loadMembers = useCallback(
    () => (admin && transfer ? accessApi.members(projectId) : Promise.resolve([])),
    [admin, projectId, transfer]
  );
  const members = useAccessResource(loadMembers, { headerRefresh: false });
  useHeaderRefresh(async () => {
    await Promise.all([resource.refresh(), members.refresh()]);
  });
  const owners = ownerCandidates(members.data, transfer?.owner_username ?? '');
  const remove = (swarm: AccessSwarm) => confirm(swarm.name, () => accessApi.deleteSwarm(swarm.id));
  const columns: Column<AccessSwarm>[] = [
    { key: 'name', label: t('substrate.name'), primary: true, render: (swarm) => swarm.name },
    {
      key: 'state',
      label: t('access.state'),
      render: (swarm) => t(`access.states.${swarm.state}`, { defaultValue: swarm.state }),
    },
    {
      key: 'owner',
      label: t('access.owner'),
      render: (swarm) => <span title={swarm.owner_id}>{swarm.owner_username}</span>,
    },
    {
      key: 'id',
      label: t('access.id'),
      mono: true,
      render: (swarm) => (
        <span title={swarm.id}>{swarm.id.length > 12 ? `…${swarm.id.slice(-12)}` : swarm.id}</span>
      ),
    },
  ];
  return (
    <div className={styles.page}>
      <PageHeader
        title={t('access.swarms')}
        meta={[project.name]}
        actions={
          <Button disabled={busy} onClick={() => setCreating(true)}>
            {t('access.create_swarm')}
          </Button>
        }
      />
      <p className={styles.sectionHint}>
        {t(admin ? 'access.admin_policy' : 'access.member_policy')}
      </p>
      <p className={styles.sectionHint}>{t('access.retry_unavailable')}</p>
      <LoadState {...resource} retry={resource.refresh}>
        <EntityTable
          columns={columns}
          rows={resource.data ?? []}
          rowKey={(swarm) => swarm.id}
          actionsLabel={t('substrate.actions')}
          actions={(swarm) => (
            <>
              {['error', 'provisioning'].includes(swarm.state) && (
                <Button
                  variant="ghost"
                  size="sm"
                  disabled={busy}
                  onClick={() =>
                    void run(() => accessApi.createSwarm(swarm.project_id, swarm.name))
                  }
                >
                  {t('access.retry_create')}
                </Button>
              )}
              {admin && (
                <Button
                  variant="ghost"
                  size="sm"
                  disabled={busy || swarm.state !== 'active'}
                  onClick={() => setTransfer(swarm)}
                >
                  {t('access.transfer')}
                </Button>
              )}
              <Button
                variant="ghost"
                size="sm"
                disabled={busy || swarm.state === 'deleted'}
                className={styles.dangerText}
                onClick={() => remove(swarm)}
              >
                {t(
                  ['error', 'deleting'].includes(swarm.state)
                    ? 'access.retry_delete'
                    : 'substrate.delete'
                )}
              </Button>
            </>
          )}
        />
        {resource.data?.length === 0 && <p>{t('access.no_swarms')}</p>}
      </LoadState>
      {creating && (
        <AccessEditor
          key={projectId}
          title={t('access.create_swarm')}
          close={() => setCreating(false)}
          fields={[{ name: 'name', label: t('substrate.name'), required: true }]}
          submit={(values) => run(() => accessApi.createSwarm(projectId, values.name.trim()))}
        />
      )}
      {transfer && admin && transfer.project_id === projectId && (
        <AccessEditor
          key={transfer.id}
          title={t('access.transfer_for', { name: transfer.name })}
          description={t('access.transfer_hint')}
          close={() => setTransfer(null)}
          fields={[
            {
              name: 'username',
              label: t('access.username'),
              type: 'select',
              required: true,
              options: owners,
              empty:
                members.error || (members.loading ? t('common.loading') : t('access.no_members')),
            },
          ]}
          submit={(values) => run(() => accessApi.transferSwarm(transfer.id, values.username))}
        />
      )}
    </div>
  );
}
