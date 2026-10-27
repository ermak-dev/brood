import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { accessApi, type AccessUser } from '@/services/brood/access';
import { useAuthStore } from '@/stores';
import { EntityTable, type Column } from '@/features/substrate/components/EntityTable';
import { PageHeader } from '@/features/substrate/components/PageHeader';
import { AccessEditor, LoadState, useAccessActions, useAccessResource } from './shared';
import styles from '@/features/substrate/substrate.module.scss';

export function UsersPage() {
  const { t } = useTranslation();
  const resource = useAccessResource(accessApi.users);
  const { run, confirm, busy } = useAccessActions(resource.refresh);
  const [editor, setEditor] = useState<
    { kind: 'create' } | { kind: 'password'; username: string } | null
  >(null);
  const columns: Column<AccessUser>[] = [
    {
      key: 'username',
      label: t('access.username'),
      primary: true,
      render: (user) => user.username,
    },
    { key: 'display_name', label: t('access.display_name'), render: (user) => user.display_name },
    {
      key: 'admin',
      label: t('access.permissions'),
      render: (user) => t(user.is_admin ? 'access.administrator' : 'access.member'),
    },
    {
      key: 'state',
      label: t('access.state'),
      render: (user) => t(user.disabled ? 'access.disabled' : 'access.enabled'),
    },
  ];
  return (
    <div className={styles.page}>
      <PageHeader
        title={t('access.users')}
        actions={
          <Button onClick={() => setEditor({ kind: 'create' })}>{t('access.create_user')}</Button>
        }
      />
      <p className={styles.sectionHint}>{t('access.admin_policy')}</p>
      <LoadState {...resource} retry={resource.refresh}>
        <EntityTable
          columns={columns}
          rows={resource.data ?? []}
          rowKey={(user) => user.username}
          actionsLabel={t('substrate.actions')}
          actions={(user) => (
            <>
              <Button
                variant="ghost"
                size="sm"
                disabled={busy}
                onClick={() =>
                  confirm(
                    user.username,
                    () => accessApi.setDisabled(user.username, !user.disabled),
                    t(user.disabled ? 'access.enable' : 'access.disable')
                  )
                }
              >
                {t(user.disabled ? 'access.enable' : 'access.disable')}
              </Button>
              <Button
                variant="ghost"
                size="sm"
                disabled={busy}
                onClick={() =>
                  confirm(
                    user.username,
                    async () => {
                      await accessApi.setRole(user.username, !user.is_admin);
                      await useAuthStore.getState().checkAuth();
                    },
                    t(user.is_admin ? 'access.revoke_admin' : 'access.grant_admin')
                  )
                }
              >
                {t(user.is_admin ? 'access.revoke_admin' : 'access.grant_admin')}
              </Button>
              <Button
                variant="ghost"
                size="sm"
                disabled={busy}
                onClick={() => setEditor({ kind: 'password', username: user.username })}
              >
                {t('access.reset_password')}
              </Button>
            </>
          )}
        />
        {resource.data?.length === 0 && <p>{t('access.empty')}</p>}
      </LoadState>
      {editor && (
        <AccessEditor
          key={editor.kind}
          title={
            editor.kind === 'create'
              ? t('access.create_user')
              : t('access.reset_password_for', { name: editor.username })
          }
          description={editor.kind === 'password' ? t('access.password_reset_hint') : undefined}
          close={() => setEditor(null)}
          fields={
            editor.kind === 'create'
              ? [
                  { name: 'username', label: t('access.username'), required: true },
                  { name: 'display_name', label: t('access.display_name') },
                  {
                    name: 'password',
                    label: t('access.password'),
                    type: 'password',
                    required: true,
                  },
                  { name: 'is_admin', label: t('access.administrator'), type: 'checkbox' },
                ]
              : [
                  {
                    name: 'password',
                    label: t('access.password'),
                    type: 'password',
                    required: true,
                  },
                ]
          }
          submit={(values) =>
            run(() =>
              editor.kind === 'create'
                ? accessApi.createUser({
                    username: values.username.trim(),
                    display_name: values.display_name ?? '',
                    password: values.password,
                    is_admin: values.is_admin === 'true',
                  })
                : accessApi.resetPassword(editor.username, values.password)
            )
          }
        />
      )}
    </div>
  );
}
