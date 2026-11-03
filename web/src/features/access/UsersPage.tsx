import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { accessApi, type AccessUser } from '@/services/brood/access';
import { useAuthStore } from '@/stores';
import { EntityPanel } from '@/features/substrate/components/EntityPanel';
import { EntityTable, type Column } from '@/features/substrate/components/EntityTable';
import { PageHeader } from '@/features/substrate/components/PageHeader';
import { panelGroups, userPlan, type ActionSpec, type UserAction } from './panelActions';
import {
  AccessEditor,
  AccountBadge,
  AccountDetails,
  LoadState,
  useAccessActions,
  useAccessResource,
} from './shared';
import { retainedRow } from './state';
import styles from '@/features/substrate/substrate.module.scss';

type Editor =
  { kind: 'create' } | { kind: 'edit'; user: AccessUser } | { kind: 'password'; username: string };

const sameUser = (row: AccessUser, open: AccessUser) => row.username === open.username;

/** Строка пользователя открывает его панель (#157); правка, сброс пароля и
 * удаление -- действия в ней. Страница открыта только администратору. */
export function UsersPage() {
  const { t } = useTranslation();
  const resource = useAccessResource(accessApi.users);
  const { run, confirm, busy } = useAccessActions(resource.refresh);
  const [editor, setEditor] = useState<Editor | null>(null);
  const [opened, setOpened] = useState<AccessUser | null>(null);
  const viewing = retainedRow(opened, resource, sameUser);
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
  // Правка и сброс пароля открывают прежний редактор вместо панели.
  const actions = (user: AccessUser): Record<UserAction, ActionSpec> => ({
    edit: {
      label: t('common.edit'),
      description: t('access.panel.edit_user_hint'),
      pending: busy,
      onSelect: () => {
        setOpened(null);
        setEditor({ kind: 'edit', user });
      },
    },
    password: {
      label: t('access.reset_password'),
      description: t('access.panel.reset_password_hint'),
      pending: busy,
      onSelect: () => {
        setOpened(null);
        setEditor({ kind: 'password', username: user.username });
      },
    },
    delete: {
      label: t('substrate.delete'),
      description: t('access.panel.delete_user_hint'),
      pending: busy,
      onSelect: () =>
        confirm(
          user.username,
          async () => {
            await accessApi.deleteUser(user.username);
            setOpened(null);
            await useAuthStore.getState().checkAuth();
          },
          t('substrate.delete'),
          t('access.confirm_user', { name: user.username })
        ),
    },
  });
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
          onRowClick={setOpened}
        />
        {resource.data?.length === 0 && <p>{t('access.empty')}</p>}
      </LoadState>
      <EntityPanel
        open={viewing !== null}
        onClose={() => setOpened(null)}
        title={viewing?.username ?? ''}
        status={viewing && <AccountBadge user={viewing} />}
        actions={viewing ? panelGroups(userPlan(), actions(viewing), t) : []}
      >
        {viewing && <AccountDetails user={viewing} />}
      </EntityPanel>
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
