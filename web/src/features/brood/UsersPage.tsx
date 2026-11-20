import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { EmptyState } from '@/components/ui/EmptyState';
import { Input } from '@/components/ui/Input';
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/Table';
import { accessApi, type User } from '@/services/brood/access';
import { useAuthStore } from '@/stores';
import { useActions, useResource } from './hooks';
import { DASH, FormModal, LoadState, PageHeader } from './ui';
import styles from './brood.module.scss';

export function UsersPage() {
  const { t } = useTranslation();
  const users = useResource(accessApi.users);
  // A change to the signed-in account (role, blocking) ends or changes its
  // session: the panel re-reads it after every mutation.
  const { run, confirm, busy } = useActions(async () => {
    await users.refresh();
    await useAuthStore.getState().checkAuth();
  });
  const [creating, setCreating] = useState(false);
  // Whose password is being reset; the name outlives the closing dialog.
  const [reset, setReset] = useState({ open: false, username: '' });
  const [username, setUsername] = useState('');
  const [displayName, setDisplayName] = useState('');
  const [password, setPassword] = useState('');
  const [admin, setAdmin] = useState(false);
  const clear = () => {
    setUsername('');
    setDisplayName('');
    setPassword('');
    setAdmin(false);
  };
  const rows = users.data ?? [];
  const action = (user: User, key: string, onClick: () => void, danger = false) => (
    <Button
      variant="ghost"
      size="sm"
      className={danger ? styles.dangerText : undefined}
      disabled={busy}
      aria-label={t(`brood.users.${key}_named`, { name: user.username })}
      onClick={onClick}
    >
      {t(`brood.users.${key}`)}
    </Button>
  );
  return (
    <div className={styles.page}>
      <PageHeader
        title={t('brood.users.title')}
        actions={
          <Button
            onClick={() => {
              clear();
              setCreating(true);
            }}
          >
            {t('brood.users.create')}
          </Button>
        }
      />
      <LoadState {...users} ready={users.data !== null} retry={users.refresh}>
        {rows.length === 0 ? (
          <EmptyState title={t('brood.users.empty')} />
        ) : (
          <Table>
            <TableHeader>
              <tr>
                <TableHead>{t('brood.users.username')}</TableHead>
                <TableHead>{t('brood.users.display_name')}</TableHead>
                <TableHead>{t('brood.users.role')}</TableHead>
                <TableHead>{t('brood.users.state')}</TableHead>
                <TableHead />
              </tr>
            </TableHeader>
            <TableBody>
              {rows.map((user) => (
                <TableRow key={user.username}>
                  <TableCell>
                    <span className={styles.nameLink}>{user.username}</span>
                  </TableCell>
                  <TableCell>{user.display_name || DASH}</TableCell>
                  <TableCell>
                    {t(user.is_admin ? 'brood.users.administrator' : 'brood.users.user')}
                  </TableCell>
                  <TableCell>
                    {t(user.disabled ? 'brood.users.disabled' : 'brood.users.enabled')}
                  </TableCell>
                  <TableCell>
                    <div className={styles.rowActions}>
                      {user.disabled
                        ? action(
                            user,
                            'enable',
                            () => void run(() => accessApi.setDisabled(user.username, false))
                          )
                        : action(user, 'disable', () =>
                            confirm({
                              title: t('brood.users.disable'),
                              message: t('brood.users.confirm_disable', { name: user.username }),
                              action: () => accessApi.setDisabled(user.username, true),
                            })
                          )}
                      {user.is_admin
                        ? action(user, 'revoke_admin', () =>
                            confirm({
                              title: t('brood.users.revoke_admin'),
                              message: t('brood.users.confirm_revoke_admin', {
                                name: user.username,
                              }),
                              action: () => accessApi.setAdministrator(user.username, false),
                            })
                          )
                        : action(user, 'grant_admin', () =>
                            confirm({
                              title: t('brood.users.grant_admin'),
                              message: t('brood.users.confirm_grant_admin', {
                                name: user.username,
                              }),
                              action: () => accessApi.setAdministrator(user.username, true),
                            })
                          )}
                      {action(user, 'reset_password', () => {
                        clear();
                        setReset({ open: true, username: user.username });
                      })}
                      {action(
                        user,
                        'delete',
                        () =>
                          confirm({
                            title: t('brood.users.delete'),
                            message: t('brood.users.confirm_delete', { name: user.username }),
                            action: () => accessApi.deleteUser(user.username),
                          }),
                        true
                      )}
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </LoadState>
      <FormModal
        open={creating}
        title={t('brood.users.create')}
        submitLabel={t('brood.create')}
        canSubmit={username.trim() !== '' && password !== ''}
        submit={() =>
          run(() =>
            accessApi.createUser({
              username: username.trim(),
              password,
              display_name: displayName.trim(),
              is_admin: admin,
            })
          )
        }
        close={() => setCreating(false)}
      >
        <Input
          label={t('brood.users.username')}
          value={username}
          autoFocus
          autoComplete="off"
          autoCapitalize="off"
          spellCheck={false}
          onChange={(event) => setUsername(event.target.value)}
        />
        <Input
          label={t('brood.users.display_name')}
          value={displayName}
          autoComplete="off"
          onChange={(event) => setDisplayName(event.target.value)}
        />
        <Input
          label={t('brood.users.password')}
          type="password"
          value={password}
          autoComplete="new-password"
          onChange={(event) => setPassword(event.target.value)}
        />
        <label className={styles.check}>
          <input
            type="checkbox"
            checked={admin}
            onChange={(event) => setAdmin(event.target.checked)}
          />
          {t('brood.users.administrator')}
        </label>
      </FormModal>
      <FormModal
        open={reset.open}
        title={t('brood.users.reset_password_for', { name: reset.username })}
        submitLabel={t('brood.users.reset_password')}
        canSubmit={password !== ''}
        submit={() => run(() => accessApi.resetPassword(reset.username, password))}
        close={() => setReset({ ...reset, open: false })}
      >
        <Input
          label={t('brood.users.new_password')}
          hint={t('brood.users.reset_password_hint')}
          type="password"
          value={password}
          autoFocus
          autoComplete="new-password"
          onChange={(event) => setPassword(event.target.value)}
        />
      </FormModal>
    </div>
  );
}
