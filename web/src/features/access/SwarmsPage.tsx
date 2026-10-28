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
import { retainedSwarm, swarmCapabilities } from './shardState';
import { SwarmAccess } from './SwarmAccess';
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
  const [selectedTransfer, setSelectedTransfer] = useState<AccessSwarm | null>(null);
  const [selectedAccess, setSelectedAccess] = useState<AccessSwarm | null>(null);
  const transfer = retainedSwarm(selectedTransfer, resource);
  const access = retainedSwarm(selectedAccess, resource);
  const canTransfer = !!transfer && swarmCapabilities(transfer, admin).own;
  const loadMembers = useCallback(
    () =>
      transfer && swarmCapabilities(transfer, admin).own
        ? accessApi.members(projectId)
        : Promise.resolve([]),
    [transfer, admin, projectId]
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
      key: 'role',
      label: t('access.role'),
      render: (swarm) => t(`access.swarm_roles.${swarm.role}`),
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
      <p className={styles.sectionHint}>{t('access.swarm_access_hint')}</p>
      <p className={styles.sectionHint}>{t('access.retry_hint')}</p>
      <LoadState {...resource} retry={resource.refresh}>
        <EntityTable
          columns={columns}
          rows={resource.data ?? []}
          rowKey={(swarm) => swarm.id}
          actionsLabel={t('substrate.actions')}
          actions={(swarm) => {
            const capability = swarmCapabilities(swarm, admin);
            return (
              <>
                {swarm.state === 'active' && (
                  <Button variant="ghost" size="sm" onClick={() => setSelectedAccess(swarm)}>
                    {t('access.swarm_access')}
                  </Button>
                )}
                {capability.retryCreate && (
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
                {capability.own && (
                  <Button
                    variant="ghost"
                    size="sm"
                    disabled={busy}
                    onClick={() => setSelectedTransfer(swarm)}
                  >
                    {t('access.transfer')}
                  </Button>
                )}
                {capability.remove && (
                  <Button
                    variant="ghost"
                    size="sm"
                    disabled={busy}
                    className={styles.dangerText}
                    onClick={() => remove(swarm)}
                  >
                    {t(
                      ['error', 'deleting'].includes(swarm.state)
                        ? 'access.retry_delete'
                        : 'substrate.delete'
                    )}
                  </Button>
                )}
              </>
            );
          }}
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
      {transfer && canTransfer && (
        <AccessEditor
          key={transfer.id}
          title={t('access.transfer_for', { name: transfer.name })}
          description={t('access.transfer_hint')}
          close={() => setSelectedTransfer(null)}
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
      {access && (
        <SwarmAccess
          key={access.id}
          swarm={access}
          close={() => setSelectedAccess(null)}
          changed={resource.refresh}
        />
      )}
    </div>
  );
}
