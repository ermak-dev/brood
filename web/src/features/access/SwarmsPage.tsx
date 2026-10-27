import { useCallback, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { useProjectStore } from '@/stores';
import { accessApi, type AccessSwarm } from '@/services/brood/access';
import { EntityTable, type Column } from '@/features/substrate/components/EntityTable';
import { PageHeader } from '@/features/substrate/components/PageHeader';
import {
  AccessEditor,
  LoadState,
  useAccessActions,
  useAccessResource,
  useAdministrator,
} from './shared';
import { ProjectSelect, useAccessProject } from './ProjectSelect';
import styles from '@/features/substrate/substrate.module.scss';

export function SwarmsPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const admin = useAdministrator();
  const selection = useAccessProject();
  const projectId = selection.project?.id ?? '';
  const setSwarm = useProjectStore((state) => state.setProject);
  const activeSwarm = useProjectStore((state) => state.project);
  const load = useCallback(
    () => (projectId ? accessApi.swarms(projectId) : Promise.resolve([])),
    [projectId]
  );
  const resource = useAccessResource(load);
  const { run, confirm, busy } = useAccessActions(resource.refresh);
  const [creating, setCreating] = useState(false);
  const [transfer, setTransfer] = useState<AccessSwarm | null>(null);
  const open = (swarm: AccessSwarm, route: string) => {
    setSwarm(swarm.id);
    navigate(route);
  };
  const remove = (swarm: AccessSwarm) =>
    confirm(swarm.name, async () => {
      await accessApi.deleteSwarm(swarm.id);
      if (activeSwarm === swarm.id) setSwarm('');
    });
  const columns: Column<AccessSwarm>[] = [
    { key: 'name', label: t('substrate.name'), primary: true, render: (swarm) => swarm.name },
    {
      key: 'state',
      label: t('access.state'),
      render: (swarm) => t(`access.states.${swarm.state}`, { defaultValue: swarm.state }),
    },
    {
      key: 'owner',
      label: t('access.owner_id'),
      mono: true,
      render: (swarm) => (
        <span title={swarm.owner_id}>
          {swarm.owner_id.length > 12 ? `…${swarm.owner_id.slice(-12)}` : swarm.owner_id}
        </span>
      ),
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
        actions={
          <Button disabled={!projectId || busy} onClick={() => setCreating(true)}>
            {t('access.create_swarm')}
          </Button>
        }
      />
      <ProjectSelect selection={selection} />
      <p className={styles.sectionHint}>
        {t(admin ? 'access.admin_policy' : 'access.member_policy')}
      </p>
      <p className={styles.sectionHint}>{t('access.retry_unavailable')}</p>
      {projectId && !selection.loading && (
        <LoadState {...resource} retry={resource.refresh}>
          <EntityTable
            columns={columns}
            rows={resource.data ?? []}
            rowKey={(swarm) => swarm.id}
            isActive={(swarm) => swarm.id === activeSwarm}
            actionsLabel={t('substrate.actions')}
            actions={(swarm) => (
              <>
                <Button
                  variant="ghost"
                  size="sm"
                  disabled={swarm.state !== 'active'}
                  onClick={() => open(swarm, '/actors')}
                >
                  {t('nav.actors')}
                </Button>
                <Button
                  variant="ghost"
                  size="sm"
                  disabled={swarm.state !== 'active'}
                  onClick={() => open(swarm, '/templates')}
                >
                  {t('nav.templates')}
                </Button>
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
                    disabled={busy}
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
      )}
      {creating && projectId && (
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
          fields={[{ name: 'username', label: t('access.username'), required: true }]}
          submit={(values) =>
            run(() => accessApi.transferSwarm(transfer.id, values.username.trim()))
          }
        />
      )}
    </div>
  );
}
