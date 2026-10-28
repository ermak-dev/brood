import { useCallback, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { useHeaderRefresh } from '@/hooks/useHeaderRefresh';
import { accessApi, type AccessProject } from '@/services/brood/access';
import { PageHeader } from '@/features/substrate/components/PageHeader';
import { EntityTable } from '@/features/substrate/components/EntityTable';
import { ProjectScope } from '@/features/substrate/components/ProjectScope';
import {
  AccessEditor,
  LoadState,
  useAccessActions,
  useAccessResource,
  useAdministrator,
} from './shared';
import { memberCandidates } from './state';
import { AccessMatrix } from './AccessMatrix';
import styles from '@/features/substrate/substrate.module.scss';

/** Пользовательские права — актуальная роль и членство проекта, не native
 * identity Substrate. Изменения состава выполняются только выбором из списка. */
export function PermissionsPage() {
  const { t } = useTranslation();
  return (
    <div className={styles.page}>
      <PageHeader title={t('access.permissions')} />
      <AccessMatrix />
      <ProjectScope title={t('access.membership')} headingLevel={2}>
        {(project) => <Members project={project} />}
      </ProjectScope>
    </div>
  );
}

function Members({ project }: { project: AccessProject }) {
  const { t } = useTranslation();
  const admin = useAdministrator();
  const projectId = project.id;
  const loadMembers = useCallback(
    () => (admin ? accessApi.members(projectId) : Promise.resolve([])),
    [admin, projectId]
  );
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
      {admin && (
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
            actionsLabel={t('substrate.actions')}
            actions={(member) => (
              <Button
                variant="ghost"
                size="sm"
                disabled={busy}
                onClick={() =>
                  confirm(
                    member.username,
                    () => accessApi.removeMember(projectId, member.username),
                    t('access.remove_member'),
                    t('access.confirm_member', { name: member.username })
                  )
                }
              >
                {t('access.remove_member')}
              </Button>
            )}
          />
          {members.data?.length === 0 && <p>{t('access.empty')}</p>}
        </LoadState>
      )}
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
