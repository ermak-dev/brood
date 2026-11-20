import { useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import {
  IconCheck,
  IconCheckCircle2,
  IconCopy,
  IconEye,
  IconEyeOff,
  IconKey,
  IconRefreshCw,
  IconShield,
  IconSidebarUsers,
  IconTrash2,
  IconUser,
} from '@/components/ui/icons';
import { Input } from '@/components/ui/Input';
import { accessApi, type User } from '@/services/brood/access';
import { useAuthStore } from '@/stores';
import { copyToClipboard } from '@/utils/clipboard';
import { useResource } from './hooks';
import { IconBan } from './icons';
import { FormSheet } from './table/dialogs';
import { PanelSection, type PanelSpec } from './table/EntityPanel';
import { useDialog, useForm } from './table/formState';
import { Truncate } from './table/format';
import { TablePage, type Column } from './table/TablePage';
import { Copyable, StatusBadge } from './ui';
import broodStyles from './brood.module.scss';
import styles from './table/table.module.scss';

const SORT = { column: 'username', desc: false };

/** Letters and digits that are not mistaken for each other when read aloud
 * or retyped: no 0/O, 1/l/I. */
const PASSWORD_ALPHABET = 'ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789';
const PASSWORD_LENGTH = 20;

/** A random password of about 116 bits from the browser's generator. */
function generatePassword() {
  const out: string[] = [];
  const bytes = new Uint8Array(PASSWORD_LENGTH * 2);
  // Bytes past the last whole multiple of the alphabet would favour its start.
  const limit = 256 - (256 % PASSWORD_ALPHABET.length);
  while (out.length < PASSWORD_LENGTH) {
    crypto.getRandomValues(bytes);
    for (const byte of bytes) {
      if (byte < limit && out.length < PASSWORD_LENGTH)
        out.push(PASSWORD_ALPHABET[byte % PASSWORD_ALPHABET.length]);
    }
  }
  return out.join('');
}

/** A password an administrator sets for somebody else: it can be generated,
 * looked at and copied to hand over, since nobody will see it again. */
function PasswordField({
  label,
  hint,
  field,
  set,
}: {
  label: string;
  hint: string;
  field: ReturnType<ReturnType<typeof useForm<'password'>>['field']>;
  set: (value: string) => void;
}) {
  const { t } = useTranslation();
  const [visible, setVisible] = useState(false);
  const [copied, setCopied] = useState(false);
  const timer = useRef<number | null>(null);
  useEffect(() => () => window.clearTimeout(timer.current ?? undefined), []);
  const copy = async () => {
    if (!(await copyToClipboard(field.value))) return;
    setCopied(true);
    window.clearTimeout(timer.current ?? undefined);
    timer.current = window.setTimeout(() => setCopied(false), 1600);
  };
  return (
    <>
      <Input
        label={label}
        hint={hint}
        required
        type={visible ? 'text' : 'password'}
        autoComplete="new-password"
        autoCapitalize="off"
        spellCheck={false}
        className={visible ? styles.mono : ''}
        {...field}
      />
      <div className={styles.fieldTools}>
        <Button
          type="button"
          variant="secondary"
          size="sm"
          onClick={() => {
            set(generatePassword());
            setVisible(true);
          }}
        >
          <span className={styles.withIcon}>
            <IconRefreshCw size={14} />
            {t('brood.users.generate')}
          </span>
        </Button>
        <Button
          type="button"
          variant="secondary"
          size="sm"
          disabled={!field.value}
          onClick={() => void copy()}
        >
          <span className={styles.withIcon} aria-live="polite">
            {copied ? <IconCheck size={14} /> : <IconCopy size={14} />}
            {copied ? t('brood.copied') : t('common.copy')}
          </span>
        </Button>
        <Button type="button" variant="ghost" size="sm" onClick={() => setVisible(!visible)}>
          <span className={styles.withIcon}>
            {visible ? <IconEyeOff size={14} /> : <IconEye size={14} />}
            {t(visible ? 'brood.users.hide_password' : 'brood.users.show_password')}
          </span>
        </Button>
      </div>
    </>
  );
}

function CreateUser({
  open,
  close,
  after,
  taken,
}: {
  open: boolean;
  close: () => void;
  after: () => unknown;
  taken: string[];
}) {
  const { t } = useTranslation();
  const [admin, setAdmin] = useState(false);
  const form = useForm({ username: '', displayName: '', password: '' }, (values) => ({
    username: !values.username.trim()
      ? t('brood.form.required')
      : taken.includes(values.username.trim())
        ? t('brood.users.username_taken')
        : undefined,
    password: values.password ? undefined : t('brood.form.required'),
  }));
  const username = form.values.username.trim();
  return (
    <FormSheet
      open={open}
      close={close}
      title={t('brood.users.create')}
      icon={IconSidebarUsers}
      description={t('brood.users.create_hint')}
      submitLabel={t('brood.users.create')}
      check={form.check}
      dirty={form.dirty || admin}
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
        hint={t('brood.users.username_hint')}
        placeholder="alice"
        required
        autoComplete="off"
        autoCapitalize="off"
        spellCheck={false}
        {...form.field('username')}
      />
      <Input
        label={t('brood.users.display_name')}
        hint={t('brood.users.display_name_hint')}
        placeholder="Alice Example"
        autoComplete="off"
        {...form.field('displayName')}
      />
      <PasswordField
        label={t('brood.users.password')}
        hint={t('brood.users.password_hint')}
        field={form.field('password')}
        set={(value) => form.set('password', value)}
      />
      <div className={styles.field}>
        <label className={styles.check}>
          <input
            type="checkbox"
            checked={admin}
            onChange={(event) => setAdmin(event.target.checked)}
          />
          {t('brood.users.administrator')}
        </label>
        <p className={styles.fieldHint}>{t('brood.users.administrator_hint')}</p>
      </div>
    </FormSheet>
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
    <FormSheet
      open={open}
      close={close}
      title={t('brood.users.reset_password_for', { name: user.username })}
      icon={IconKey}
      description={t('brood.users.reset_password_hint')}
      submitLabel={t('brood.users.reset_password')}
      check={form.check}
      dirty={form.dirty}
      submit={() => accessApi.resetPassword(user.username, form.values.password)}
      success={t('brood.users.password_reset', { name: user.username })}
      after={after}
    >
      <PasswordField
        label={t('brood.users.new_password')}
        hint={t('brood.users.password_hint')}
        field={form.field('password')}
        set={(value) => form.set('password', value)}
      />
    </FormSheet>
  );
}

export function UsersPage() {
  const { t } = useTranslation();
  const users = useResource(accessApi.users);
  const self = useAuthStore((state) => state.username);
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
  const badge = (user: User) => (
    <StatusBadge
      tone={user.disabled ? 'problem' : 'progress'}
      icon={user.disabled ? IconBan : IconCheckCircle2}
    >
      {stateLabel(user)}
    </StatusBadge>
  );

  const columns: Column<User>[] = [
    {
      id: 'username',
      header: t('brood.users.username'),
      primary: true,
      cell: (row) => row.username,
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
      cell: (row) => (
        <span className={styles.withIcon}>
          {row.is_admin ? <IconShield size={14} /> : <IconUser size={14} />}
          {roleLabel(row)}
        </span>
      ),
      text: roleLabel,
      sort: (row) => (row.is_admin ? 0 : 1),
    },
    {
      id: 'state',
      header: t('brood.users.state'),
      cell: badge,
      text: stateLabel,
      sort: (row) => (row.disabled ? 0 : 1),
    },
  ];

  const panel = (row: User): PanelSpec => {
    const name = row.username;
    return {
      icon: row.is_admin ? IconShield : IconUser,
      title: name,
      badge: badge(row),
      status: (
        <>
          <p className={styles.statusLine}>
            <span>
              <b>{roleLabel(row)}</b>
            </span>
            {row.display_name && <span>{row.display_name}</span>}
          </p>
          {name === self && <p className={broodStyles.notice}>{t('brood.users.self')}</p>}
        </>
      ),
      groups: [
        {
          id: 'work',
          label: t('brood.panel.actions'),
          actions: [
            {
              id: 'reset_password',
              label: t('brood.users.reset_password'),
              icon: IconKey,
              description: t('brood.users.reset_password_hint'),
              onSelect: () => reset.show(row),
            },
            row.is_admin
              ? {
                  id: 'revoke_admin',
                  label: t('brood.users.revoke_admin'),
                  icon: IconUser,
                  description: t('brood.users.revoke_admin_hint'),
                  confirm: t('brood.users.confirm_revoke_admin', { name }),
                  run: () => accessApi.setAdministrator(name, false),
                }
              : {
                  id: 'grant_admin',
                  label: t('brood.users.grant_admin'),
                  icon: IconShield,
                  description: t('brood.users.grant_admin_hint'),
                  confirm: t('brood.users.confirm_grant_admin', { name }),
                  run: () => accessApi.setAdministrator(name, true),
                },
            row.disabled
              ? {
                  id: 'enable',
                  label: t('brood.users.enable'),
                  icon: IconCheckCircle2,
                  description: t('brood.users.enable_hint'),
                  run: () => accessApi.setDisabled(name, false),
                }
              : {
                  id: 'disable',
                  label: t('brood.users.disable'),
                  icon: IconBan,
                  description: t('brood.users.disable_hint'),
                  confirm: t('brood.users.confirm_disable', { name }),
                  run: () => accessApi.setDisabled(name, true),
                },
          ],
        },
        {
          id: 'danger',
          label: t('brood.panel.danger'),
          danger: true,
          actions: [
            {
              id: 'delete',
              label: t('brood.users.delete'),
              icon: IconTrash2,
              description: t('brood.users.delete_hint'),
              tone: 'danger',
              confirm: t('brood.users.confirm_delete', { name }),
              run: () => accessApi.deleteUser(name),
            },
          ],
        },
      ],
      details: (
        <PanelSection title={t('brood.panel.details')}>
          <dl className={broodStyles.details}>
            <dt>{t('brood.users.username')}</dt>
            <dd>
              <Copyable value={name} />
            </dd>
            <dt>{t('brood.users.display_name')}</dt>
            <dd>{row.display_name || '—'}</dd>
            <dt>{t('brood.users.role')}</dt>
            <dd>{roleLabel(row)}</dd>
          </dl>
        </PanelSection>
      ),
    };
  };

  return (
    <TablePage
      title={t('brood.users.title')}
      icon={IconSidebarUsers}
      description={t('brood.users.description')}
      create={{
        label: t('brood.users.create'),
        sheet: (dialog) => (
          <CreateUser
            {...dialog}
            after={refresh}
            taken={(users.data ?? []).map((user) => user.username)}
          />
        ),
      }}
      resource={users}
      rowKey={(row) => row.username}
      rowName={(row) => row.username}
      columns={columns}
      panel={panel}
      defaultSort={SORT}
      empty={t('brood.users.empty')}
      afterAction={refresh}
    >
      {reset.mount((dialog, user) => (
        <ResetPassword {...dialog} user={user} after={refresh} />
      ))}
    </TablePage>
  );
}
