import { useCallback, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { useHeaderRefresh } from '@/hooks/useHeaderRefresh';
import { accessApi, type AccessProject, type AccessUser } from '@/services/brood/access';
import { EntityPanel } from '@/features/substrate/components/EntityPanel';
import { PageHeader } from '@/features/substrate/components/PageHeader';
import { EntityTable } from '@/features/substrate/components/EntityTable';
import { memberPlan, panelGroups, type ActionSpec, type MemberAction } from './panelActions';
import {
  AccessEditor,
  AccountBadge,
  AccountDetails,
  LoadState,
  useAccessActions,
  useAccessResource,
  useAdministrator,
} from './shared';
import { memberCandidates, retainedRow } from './state';
import styles from '@/features/substrate/substrate.module.scss';

const sameMember = (row: AccessUser, open: AccessUser) => row.username === open.username;

/** Состав проекта открыт его участникам; меняет его только администратор.
 * Строка участника открывает его панель (#157): у участника она только для
 * чтения, у администратора в ней исключение из проекта. */
export function ProjectMembers({ project }: { project: AccessProject }) {
  const { t } = useTranslation();
  const admin = useAdministrator();
  const projectId = project.id;
  const loadMembers = useCallback(() => accessApi.members(projectId), [projectId]);
  const members = useAccessResource(loadMembers, { headerRefresh: false });
  const { run, confirm, busy } = useAccessActions(members.refresh);
  const [adding, setAdding] = useState(false);
  const loadUsers = useCallback(
    () => (admin && adding ? accessApi.users() : Promise.resolve([])),
    [admin, adding]
  );
  const users = useAccessResource(loadUsers, { headerRefresh: false });
  useHeaderRefresh(async () => {
    await Promise.all([members.refresh(), users.refresh()]);
  });
  const candidates = memberCandidates(users.data, members.data);
  const [opened, setOpened] = useState<AccessUser | null>(null);
  const viewing = retainedRow(opened, members, sameMember);
  const actions = (member: AccessUser): Record<MemberAction, ActionSpec> => ({
    remove: {
      label: t('access.remove_member'),
      description: t('access.panel.remove_member_hint'),
      pending: busy,
      onSelect: () =>
        confirm(
          member.username,
          async () => {
            await accessApi.removeMember(projectId, member.username);
            setOpened(null);
          },
          t('access.remove_member'),
          t('access.confirm_member', { name: member.username })
        ),
    },
  });
  return (
    <section className={styles.page}>
      <PageHeader
        title={t('access.membership')}
        headingLevel={2}
        meta={[project.name]}
        actions={
          admin && (
            <Button disabled={busy} onClick={() => setAdding(true)}>
              {t('access.add_member')}
            </Button>
          )
        }
      />
      <LoadState {...members} retry={members.refresh}>
        <EntityTable
          rows={members.data ?? []}
          rowKey={(member) => member.username}
          columns={[
            {
              key: 'username',
              label: t('access.username'),
              primary: true,
              render: (member) => member.username,
            },
            {
              key: 'display_name',
              label: t('access.display_name'),
              render: (member) => member.display_name,
            },
            {
              key: 'role',
              label: t('access.role'),
              render: (member) => t(member.is_admin ? 'access.administrator' : 'access.user'),
            },
            {
              key: 'state',
              label: t('access.state'),
              render: (member) => t(member.disabled ? 'access.disabled' : 'access.enabled'),
            },
          ]}
          onRowClick={setOpened}
        />
        {members.data?.length === 0 && <p>{t('access.empty')}</p>}
      </LoadState>
      <EntityPanel
        open={viewing !== null}
        onClose={() => setOpened(null)}
        eyebrow={project.name}
        title={viewing?.username ?? ''}
        status={viewing && <AccountBadge user={viewing} />}
        actions={viewing ? panelGroups(memberPlan(admin), actions(viewing), t) : []}
      >
        {viewing && <AccountDetails user={viewing} />}
      </EntityPanel>
      {adding && admin && (
        <AccessEditor
          key={projectId}
          title={t('access.add_member')}
          description={project.name}
          close={() => setAdding(false)}
          fields={[
            {
              name: 'username',
              label: t('access.username'),
              type: 'select',
              required: true,
              options: candidates,
              empty:
                users.error ||
                members.error ||
                (users.loading || members.loading
                  ? t('common.loading')
                  : t('access.no_candidates')),
            },
          ]}
          submit={(values) => run(() => accessApi.addMember(projectId, values.username))}
        />
      )}
    </section>
  );
}
