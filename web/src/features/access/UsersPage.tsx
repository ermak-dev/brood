import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { accessApi, type AccessUser } from '@/services/brood/access';
import { useAuthStore } from '@/stores';
import { EntityTable, type Column } from '@/features/substrate/components/EntityTable';
import { PageHeader } from '@/features/substrate/components/PageHeader';
import { AccessEditor, LoadState, useAccessActions, useAccessResource } from './shared';
import styles from '@/features/substrate/substrate.module.scss';

type Editor =
  { kind: 'create' } | { kind: 'edit'; user: AccessUser } | { kind: 'password'; username: string };

export function UsersPage() {
  const { t } = useTranslation();
  const resource = useAccessResource(accessApi.users);
  const { run, confirm, busy } = useAccessActions(resource.refresh);
  const [editor, setEditor] = useState<Editor | null>(null);
  const editing = editor?.kind === 'edit' ? editor.user : null;
  const initialValues = editing
    ? {
        username: editing.username,
        display_name: editing.display_name,
        is_admin: String(editing.is_admin),
        disabled: String(editing.disabled),
      }
    : { username: '', display_name: '', is_admin: 'false', disabled: 'false' };
  const columns: Column<AccessUser>[] = [
    {
      key: 'username',
      label: t('access.username'),
      primary: true,
      render: (user) => user.username,
    },
    { key: 'display_name', label: t('access.display_name'), render: (user) => user.display_name },
    {
      key: 'role',
      label: t('access.role'),
      render: (user) => t(user.is_admin ? 'access.administrator' : 'access.user'),
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
      <LoadState {...resource} retry={resource.refresh}>
        <EntityTable
          columns={columns}
          rows={resource.data ?? []}
          rowKey={(user) => user.username}
          onRowClick={(user) => setEditor({ kind: 'edit', user })}
          actionsLabel={t('substrate.actions')}
          actions={(user) => (
            <>
              <Button
                variant="ghost"
                size="sm"
                disabled={busy}
                onClick={() => setEditor({ kind: 'edit', user })}
              >
                {t('common.edit')}
              </Button>
              <Button
                variant="ghost"
                size="sm"
                disabled={busy}
                onClick={() => setEditor({ kind: 'password', username: user.username })}
              >
                {t('access.reset_password')}
              </Button>
              <Button
                variant="ghost"
                size="sm"
                disabled={busy}
                className={styles.dangerText}
                onClick={() =>
                  confirm(
                    user.username,
                    async () => {
                      await accessApi.deleteUser(user.username);
                      await useAuthStore.getState().checkAuth();
                    },
                    t('substrate.delete'),
                    t('access.confirm_user', { name: user.username })
                  )
                }
              >
                {t('substrate.delete')}
              </Button>
            </>
          )}
        />
        {resource.data?.length === 0 && <p>{t('access.empty')}</p>}
      </LoadState>
      {editor && (
        <AccessEditor
          key={`${editor.kind}-${editing?.username ?? (editor.kind === 'password' ? editor.username : '')}`}
          title={
            editor.kind === 'password'
              ? t('access.reset_password_for', { name: editor.username })
              : editing
                ? t('access.edit_user', { name: editing.username })
                : t('access.create_user')
          }
          description={
            editor.kind === 'password'
              ? t('access.password_reset_hint')
              : t('access.user_properties_hint')
          }
          close={() => setEditor(null)}
          initialValues={editor.kind === 'password' ? {} : initialValues}
          fields={
            editor.kind === 'password'
              ? [
                  {
                    name: 'password',
                    label: t('access.password'),
                    type: 'password',
                    required: true,
                  },
                ]
              : [
                  {
                    name: 'username',
                    label: t('access.username'),
                    required: true,
                    disabled: Boolean(editing),
                  },
                  { name: 'display_name', label: t('access.display_name') },
                  ...(editor.kind === 'create'
                    ? [
                        {
                          name: 'password',
                          label: t('access.password'),
                          type: 'password' as const,
                          required: true,
                        },
                      ]
                    : []),
                  {
                    name: 'is_admin',
                    label: t('access.role'),
                    type: 'select',
                    required: true,
                    options: [
                      { value: 'false', label: t('access.user') },
                      { value: 'true', label: t('access.administrator') },
                    ],
                  },
                  { name: 'disabled', label: t('access.disabled'), type: 'checkbox' },
                ]
          }
          submit={async (values) => {
            const ok = await run(() => {
              if (editor.kind === 'password')
                return accessApi.resetPassword(editor.username, values.password);
              const properties = {
                display_name: values.display_name ?? '',
                is_admin: values.is_admin === 'true',
                disabled: values.disabled === 'true',
              };
              if (editing) return accessApi.updateUser(editing.username, properties);
              return accessApi.createUser({
                username: values.username.trim(),
                password: values.password,
                ...properties,
              });
            });
            if (ok && editing) await useAuthStore.getState().checkAuth();
            return ok;
          }}
        />
      )}
    </div>
  );
}
