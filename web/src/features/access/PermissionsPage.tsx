import { useCallback, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { useHeaderRefresh } from '@/hooks/useHeaderRefresh';
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
import { memberCandidates, projectSectionPath } from './state';
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
  const members = useAccessResource(loadMembers, { headerRefresh: false });
  const { run, confirm, busy } = useAccessActions(members.refresh);
  const [adding, setAdding] = useState(false);
  // Кандидаты в участники: пользователи установки, кроме уже добавленных и
  // заблокированных. Список открыт только администратору -- он же и добавляет.
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
        {/* Исключение живёт в строке участника: вводить имя руками, чтобы
            удалить видимую строку, незачем. */}
        {admin && (
          <div className={styles.rowActions}>
            <Button disabled={busy} onClick={() => setAdding(true)}>
              {t('access.add_member')}
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
      </Section>
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
              // Пока список грузится или не пришёл, поле говорит именно это,
              // а не «кандидатов нет».
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
    </div>
  );
}
