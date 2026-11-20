import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Input } from '@/components/ui/Input';
import { accessApi, type User } from '@/services/brood/access';
import { useAuthStore } from '@/stores';
import { useResource } from './hooks';
import { FormDialog } from './table/dialogs';
import { useDialog, useForm } from './table/formState';
import { Truncate } from './table/format';
import { TablePage, type Column, type RowAction } from './table/TablePage';
import { StatusBadge } from './ui';
import styles from './table/table.module.scss';

const SORT = { column: 'username', desc: false };

function CreateUser({
  open,
  close,
  after,
}: {
  open: boolean;
  close: () => void;
  after: () => unknown;
}) {
  const { t } = useTranslation();
  const [admin, setAdmin] = useState(false);
  const form = useForm({ username: '', displayName: '', password: '' }, (values) => ({
    username: values.username.trim() ? undefined : t('brood.form.required'),
    password: values.password ? undefined : t('brood.form.required'),
  }));
  const username = form.values.username.trim();
  return (
    <FormDialog
      open={open}
      close={close}
      title={t('brood.users.create')}
      submitLabel={t('brood.form.create')}
      valid={form.valid}
      submit={() =>
        accessApi.createUser({
          username,
          password: form.values.password,
          display_name: form.values.displayName.trim(),
          is_admin: admin,
        })
      }
      success={t('brood.users.created', { name: username })}
      after={after}
    >
      <Input
        label={t('brood.users.username')}
        required
        autoComplete="off"
        autoCapitalize="off"
        spellCheck={false}
        {...form.field('username')}
      />
      <Input
        label={t('brood.users.display_name')}
        autoComplete="off"
        {...form.field('displayName')}
      />
      <Input
        label={t('brood.users.password')}
        required
        type="password"
        autoComplete="new-password"
        {...form.field('password')}
      />
      <label className={styles.check}>
        <input
          type="checkbox"
          checked={admin}
          onChange={(event) => setAdmin(event.target.checked)}
        />
        {t('brood.users.administrator')}
      </label>
    </FormDialog>
  );
}

function ResetPassword({
  user,
  open,
  close,
  after,
}: {
  user: User;
  open: boolean;
  close: () => void;
  after: () => unknown;
}) {
  const { t } = useTranslation();
  const form = useForm({ password: '' }, ({ password }) => ({
    password: password ? undefined : t('brood.form.required'),
  }));
  return (
    <FormDialog
      open={open}
      close={close}
      title={t('brood.users.reset_password_for', { name: user.username })}
      submitLabel={t('brood.users.reset_password')}
      valid={form.valid}
      submit={() => accessApi.resetPassword(user.username, form.values.password)}
      success={t('brood.users.password_reset', { name: user.username })}
      after={after}
    >
      <Input
        label={t('brood.users.new_password')}
        hint={t('brood.users.reset_password_hint')}
        required
        type="password"
        autoComplete="new-password"
        {...form.field('password')}
      />
    </FormDialog>
  );
}

export function UsersPage() {
  const { t } = useTranslation();
  const users = useResource(accessApi.users);
  const create = useDialog();
  const reset = useDialog<User>();
  // A change to the signed-in account (role, blocking) ends or changes its
  // session: the panel re-reads it after every mutation.
  const refresh = async () => {
    await users.refresh();
    await useAuthStore.getState().checkAuth();
  };
  const roleLabel = (user: User) =>
    t(user.is_admin ? 'brood.users.administrator' : 'brood.users.user');
  const stateLabel = (user: User) =>
    t(user.disabled ? 'brood.users.disabled' : 'brood.users.enabled');

  const columns: Column<User>[] = [
    {
      id: 'username',
      header: t('brood.users.username'),
      cell: (row) => <span className={styles.name}>{row.username}</span>,
      text: (row) => row.username,
      sort: (row) => row.username,
    },
    {
      id: 'display_name',
      header: t('brood.users.display_name'),
      cell: (row) => <Truncate text={row.display_name} />,
      text: (row) => row.display_name,
      sort: (row) => row.display_name,
    },
    {
      id: 'role',
      header: t('brood.users.role'),
      cell: roleLabel,
      text: roleLabel,
      sort: (row) => (row.is_admin ? 0 : 1),
    },
    {
      id: 'state',
      header: t('brood.users.state'),
      cell: (row) => (
        <StatusBadge tone={row.disabled ? 'problem' : 'neutral'}>{stateLabel(row)}</StatusBadge>
      ),
      text: stateLabel,
      sort: (row) => (row.disabled ? 0 : 1),
    },
  ];

  const actions = (row: User): RowAction[] => {
    const name = row.username;
    return [
      {
        id: 'reset_password',
        label: t('brood.users.reset_password'),
        inline: true,
        open: () => reset.show(row),
      },
      row.is_admin
        ? {
            id: 'revoke_admin',
            label: t('brood.users.revoke_admin'),
            confirm: t('brood.users.confirm_revoke_admin', { name }),
            run: () => accessApi.setAdministrator(name, false),
          }
        : {
            id: 'grant_admin',
            label: t('brood.users.grant_admin'),
            confirm: t('brood.users.confirm_grant_admin', { name }),
            run: () => accessApi.setAdministrator(name, true),
          },
      row.disabled
        ? {
            id: 'enable',
            label: t('brood.users.enable'),
            run: () => accessApi.setDisabled(name, false),
          }
        : {
            id: 'disable',
            label: t('brood.users.disable'),
            danger: true,
            confirm: t('brood.users.confirm_disable', { name }),
            run: () => accessApi.setDisabled(name, true),
          },
      {
        id: 'delete',
        label: t('common.delete'),
        danger: true,
        confirm: t('brood.users.confirm_delete', { name }),
        run: () => accessApi.deleteUser(name),
      },
    ];
  };

  return (
    <TablePage
      title={t('brood.users.title')}
      description={t('brood.users.description')}
      primary={{ label: t('brood.users.create'), onClick: () => create.show(null) }}
      resource={users}
      rowKey={(row) => row.username}
      rowName={(row) => row.username}
      columns={columns}
      defaultSort={SORT}
      empty={t('brood.users.empty')}
      actions={actions}
      afterAction={refresh}
    >
      {create.mount((dialog) => (
        <CreateUser {...dialog} after={refresh} />
      ))}
      {reset.mount((dialog, user) => (
        <ResetPassword {...dialog} user={user} after={refresh} />
      ))}
    </TablePage>
  );
}
