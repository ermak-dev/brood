import { useCallback, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { accessApi, type AccessProject } from '@/services/brood/access';
import { PageHeader } from '@/features/substrate/components/PageHeader';
import { Section } from '@/features/substrate/components/form';
import { EntityTable } from '@/features/substrate/components/EntityTable';
import {
  AccessEditor,
  LoadState,
  useAccessActions,
  useAccessResource,
  useAdministrator,
} from './shared';
import { projectSectionPath } from './state';
import { ProjectRoute } from './useRouteProject';
import styles from '@/features/substrate/substrate.module.scss';

/** brood: участники проекта из маршрута. Состав читает и меняет только
 * администратор; участник видит модель доступа и свои проекты. */
export function PermissionsPage() {
  const { t } = useTranslation();
  return (
    <ProjectRoute title={t('access.membership')}>
      {(project) => <Members project={project} />}
    </ProjectRoute>
  );
}

function Members({ project }: { project: AccessProject }) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const admin = useAdministrator();
  const projectId = project.id;
  // Список участников отдаётся только администратору: обычному участнику
  // панель его не запрашивает.
  const loadMembers = useCallback(
    () => (admin ? accessApi.members(projectId) : Promise.resolve([])),
    [admin, projectId]
  );
  const members = useAccessResource(loadMembers);
  const { run, confirm, busy } = useAccessActions(members.refresh);
  const [editor, setEditor] = useState<'add' | 'remove' | null>(null);
  return (
    <div className={styles.page}>
      <PageHeader
        title={t('access.membership')}
        meta={[project.name]}
        actions={
          <Button
            variant="secondary"
            onClick={() => navigate(projectSectionPath('swarms', projectId))}
          >
            {t('access.swarms')}
          </Button>
        }
      />
      <Section title={t(admin ? 'access.administrator' : 'access.member')}>
        <p>{t(admin ? 'access.admin_policy' : 'access.member_policy')}</p>
        <p>{t('access.server_authority')}</p>
      </Section>
      <Section title={t('access.membership')}>
        {!admin && <p>{t('access.membership_hint')}</p>}
        {admin && (
          <div className={styles.rowActions}>
            <Button disabled={busy} onClick={() => setEditor('add')}>
              {t('access.add_member')}
            </Button>
            <Button variant="secondary" disabled={busy} onClick={() => setEditor('remove')}>
              {t('access.remove_member')}
            </Button>
          </div>
        )}
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
                      t('access.remove_member')
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
      </Section>
      {editor && admin && (
        <AccessEditor
          key={`${projectId}-${editor}`}
          title={t(editor === 'add' ? 'access.add_member' : 'access.remove_member')}
          description={project.name}
          close={() => setEditor(null)}
          fields={[{ name: 'username', label: t('access.username'), required: true }]}
          submit={async (values) => {
            const username = values.username.trim();
            if (editor === 'add') return run(() => accessApi.addMember(projectId, username));
            confirm(
              username,
              () => accessApi.removeMember(projectId, username),
              t('access.remove_member')
            );
            return true;
          }}
        />
      )}
    </div>
  );
}
