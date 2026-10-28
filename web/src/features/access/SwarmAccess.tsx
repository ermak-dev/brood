import { useCallback, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import {
  accessApi,
  type AccessSwarm,
  type SwarmMember,
  type SwarmRole,
} from '@/services/brood/access';
import { EditorSheet } from '@/features/substrate/components/EditorSheet';
import { EntityTable } from '@/features/substrate/components/EntityTable';
import {
  AccessEditor,
  LoadState,
  useAccessActions,
  useAccessResource,
  useAdministrator,
} from './shared';
import { canRemoveGrant, grantCandidates, swarmCapabilities } from './shardState';
import styles from '@/features/substrate/substrate.module.scss';

export function SwarmAccess({
  swarm,
  close,
  changed,
}: {
  swarm: AccessSwarm;
  close: () => void;
  changed: () => Promise<void>;
}) {
  const { t } = useTranslation();
  const admin = useAdministrator();
  const canOwn = swarmCapabilities(swarm, admin).own;
  const load = useCallback(() => accessApi.swarmMembers(swarm.id), [swarm.id]);
  const grants = useAccessResource(load);
  const loadMembers = useCallback(
    () => (canOwn ? accessApi.members(swarm.project_id) : Promise.resolve([])),
    [canOwn, swarm.project_id]
  );
  const members = useAccessResource(loadMembers);
  const refresh = async () => {
    await Promise.all([grants.refresh(), changed()]);
  };
  const { run, confirm, busy } = useAccessActions(refresh);
  const [editing, setEditing] = useState<SwarmMember | 'new' | null>(null);
  const candidates = grantCandidates(members.data, grants.data);
  const roles: SwarmRole[] = ['owner', 'editor', 'viewer'];
  const options = roles
    .filter(
      (role) =>
        !editing ||
        editing === 'new' ||
        editing.role !== 'owner' ||
        canRemoveGrant(grants.data, editing.username) ||
        role === 'owner'
    )
    .map((role) => ({ value: role, label: t(`access.swarm_roles.${role}`) }));
  return (
    <>
      {(!editing || !canOwn) && (
        <EditorSheet open onClose={close} title={t('access.swarm_access')} description={swarm.name}>
          <div className={styles.form}>
            <p className={styles.sectionHint}>{t('access.swarm_access_hint')}</p>
            {canOwn && (
              <Button disabled={busy} onClick={() => setEditing('new')}>
                {t('access.add_member')}
              </Button>
            )}
            <LoadState {...grants} retry={grants.refresh}>
              <EntityTable
                rows={grants.data ?? []}
                rowKey={(member) => member.user_id}
                columns={[
                  {
                    key: 'username',
                    label: t('access.username'),
                    primary: true,
                    render: (member) => member.username,
                  },
                  {
                    key: 'role',
                    label: t('access.role'),
                    render: (member) => t(`access.swarm_roles.${member.role}`),
                  },
                  {
                    key: 'state',
                    label: t('access.state'),
                    render: (member) => t(member.disabled ? 'access.disabled' : 'access.enabled'),
                  },
                ]}
                actionsLabel={t('substrate.actions')}
                actions={
                  canOwn
                    ? (member) => (
                        <>
                          <Button
                            variant="ghost"
                            size="sm"
                            disabled={busy || member.disabled}
                            onClick={() => setEditing(member)}
                          >
                            {t('substrate.edit')}
                          </Button>
                          <Button
                            variant="ghost"
                            size="sm"
                            disabled={busy || !canRemoveGrant(grants.data, member.username)}
                            onClick={() =>
                              confirm(
                                member.username,
                                () => accessApi.removeSwarmMember(swarm.id, member.username),
                                t('access.remove_member'),
                                t('access.confirm_swarm_member', { name: member.username })
                              )
                            }
                          >
                            {t('access.remove_member')}
                          </Button>
                        </>
                      )
                    : undefined
                }
              />
            </LoadState>
          </div>
        </EditorSheet>
      )}
      {editing && canOwn && (
        <AccessEditor
          key={editing === 'new' ? 'new' : editing.username}
          title={t(editing === 'new' ? 'access.add_member' : 'substrate.edit')}
          close={() => setEditing(null)}
          initialValues={
            editing === 'new'
              ? { role: 'viewer' }
              : { username: editing.username, role: editing.role }
          }
          fields={[
            {
              name: 'username',
              label: t('access.username'),
              type: 'select',
              required: true,
              disabled: editing !== 'new',
              options:
                editing === 'new'
                  ? candidates
                  : [{ value: editing.username, label: editing.username }],
              empty:
                members.error ||
                (members.loading ? t('common.loading') : t('access.no_candidates')),
            },
            { name: 'role', label: t('access.role'), type: 'select', required: true, options },
          ]}
          submit={(values) =>
            run(() => accessApi.setSwarmMember(swarm.id, values.username, values.role as SwarmRole))
          }
        />
      )}
    </>
  );
}
