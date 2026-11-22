import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Input } from '@/components/ui/Input';
import { IconPencil, IconShield, IconTrash2 } from '@/components/ui/icons';
import {
  DELEGATED_ROLE_PERMISSIONS,
  ROLE_PERMISSIONS,
  rolesApi,
  type Role,
} from '@/services/brood/roles';
import { useAdministrator, useResource } from './hooks';
import { Field, FormSection, FormSheet } from './table/dialogs';
import { PanelSection, type PanelSpec } from './table/EntityPanel';
import { useDialog, useForm } from './table/formState';
import { TablePage, type Column } from './table/TablePage';
import { StatusBadge } from './ui';
import styles from './table/table.module.scss';

const SORT = { column: 'name', desc: false };

function RoleForm({
  open,
  close,
  after,
  role,
}: {
  open: boolean;
  close: () => void;
  after: () => unknown;
  role?: Role;
}) {
  const { t } = useTranslation();
  const [permissions, setPermissions] = useState(role?.permissions ?? []);
  const form = useForm({ name: role?.name ?? '', prompt: role?.prompt ?? '' }, (values) => ({
    name: values.name.trim() ? undefined : t('brood.form.required'),
  }));
  const originalPermissions = role?.permissions ?? [];
  const permissionsChanged =
    permissions.length !== originalPermissions.length ||
    permissions.some((permission) => !originalPermissions.includes(permission));

  return (
    <FormSheet
      open={open}
      close={close}
      icon={IconShield}
      title={t(role ? 'brood.roles.edit' : 'brood.roles.create')}
      description={t(role ? 'brood.roles.edit_hint' : 'brood.roles.create_hint')}
      submitLabel={t(role ? 'common.save' : 'brood.roles.create')}
      check={form.check}
      dirty={form.dirty || permissionsChanged}
      submit={() => {
        const body = { name: form.values.name.trim(), prompt: form.values.prompt, permissions };
        return role ? rolesApi.update(role.id, body) : rolesApi.create(body);
      }}
      success={t(role ? 'brood.roles.saved' : 'brood.roles.created', {
        name: form.values.name.trim(),
      })}
      after={after}
    >
      <Input
        label={t('brood.name')}
        hint={t('brood.roles.name_hint')}
        required
        autoComplete="off"
        {...form.field('name')}
      />
      <Field label={t('brood.roles.prompt')} hint={t('brood.roles.prompt_hint')}>
        {(id) => (
          <textarea
            id={id}
            className={`form-input ${styles.textarea}`}
            value={form.values.prompt}
            onChange={(event) => form.set('prompt', event.target.value)}
          />
        )}
      </Field>
      <FormSection
        title={t('brood.roles.permissions')}
        hint={t(
          role?.default ? 'brood.roles.default_permissions_hint' : 'brood.roles.permissions_hint'
        )}
      >
        {!role?.default && <p className={styles.fieldHint}>{t('brood.roles.tail_scope_hint')}</p>}
        <div className={styles.grid2}>
          {ROLE_PERMISSIONS.map((permission) => (
            <label key={permission} className={styles.check}>
              <input
                type="checkbox"
                checked={permissions.includes(permission)}
                disabled={role?.default && DELEGATED_ROLE_PERMISSIONS.has(permission)}
                onChange={(event) => {
                  const checked = event.target.checked;
                  setPermissions((before) => {
                    if (!checked) return before.filter((key) => key !== permission);
                    const allowed = permission.startsWith('tail:')
                      ? before.filter((key) => !key.startsWith('tail:'))
                      : before;
                    return [...allowed, permission];
                  });
                }}
              />
              {t(`brood.roles.permission.${permission}`, { nsSeparator: false })}
            </label>
          ))}
        </div>
      </FormSection>
    </FormSheet>
  );
}

export function RolesPage() {
  const { t } = useTranslation();
  const administrator = useAdministrator();
  const roles = useResource(rolesApi.list);
  const edit = useDialog<Role>();
  const permissionLabel = (key: string) =>
    t(`brood.roles.permission.${key}`, { defaultValue: key, nsSeparator: false });
  const columns: Column<Role>[] = [
    {
      id: 'name',
      header: t('brood.name'),
      primary: true,
      cell: (row) => (
        <span className={styles.withIcon}>
          {row.name}
          {row.default && <StatusBadge tone="neutral">{t('brood.roles.default')}</StatusBadge>}
        </span>
      ),
      text: (row) => row.name,
      sort: (row) => row.name,
    },
  ];
  if (administrator) {
    columns.push({
      id: 'agents',
      header: t('brood.agents.title'),
      numeric: true,
      cell: (row) => row.agents.length,
      sort: (row) => row.agents.length,
    });
  }
  const panel = (row: Role): PanelSpec => ({
    icon: IconShield,
    title: row.name,
    badge: row.default ? (
      <StatusBadge tone="neutral">{t('brood.roles.default')}</StatusBadge>
    ) : null,
    groups: administrator
      ? [
          {
            id: 'work',
            label: t('brood.panel.actions'),
            actions: [
              {
                id: 'edit',
                label: t('brood.roles.edit'),
                description: t('brood.roles.edit_hint'),
                icon: IconPencil,
                onSelect: () => edit.show(row),
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
                label: t('brood.roles.delete'),
                description: t('brood.roles.delete_hint'),
                icon: IconTrash2,
                tone: 'danger',
                blocked: row.default
                  ? t('brood.roles.cannot_delete_default')
                  : row.agents.length > 0
                    ? t('brood.roles.cannot_delete_assigned')
                    : undefined,
                confirm: t('brood.roles.confirm_delete', {
                  name: row.name,
                }),
                run: () => rolesApi.remove(row.id),
              },
            ],
          },
        ]
      : [],
    details: (
      <>
        {administrator && (
          <PanelSection title={t('brood.roles.prompt')}>
            {row.prompt ? (
              <pre className={styles.log}>{row.prompt}</pre>
            ) : (
              <p className={styles.fieldHint}>{t('brood.roles.no_prompt')}</p>
            )}
          </PanelSection>
        )}
        <PanelSection title={t('brood.roles.permissions')}>
          {row.permissions.length ? (
            <ul className={styles.items}>
              {row.permissions.map((key) => (
                <li key={key}>{permissionLabel(key)}</li>
              ))}
            </ul>
          ) : (
            <p className={styles.fieldHint}>{t('brood.roles.no_permissions')}</p>
          )}
        </PanelSection>
        {administrator && (
          <PanelSection title={t('brood.agents.title')}>
            {row.agents.length ? (
              <ul className={styles.items}>
                {row.agents.map((agent, index) => (
                  <li key={`${agent}/${index}`}>{agent}</li>
                ))}
              </ul>
            ) : (
              <p className={styles.fieldHint}>{t('brood.roles.no_agents')}</p>
            )}
          </PanelSection>
        )}
      </>
    ),
  });

  return (
    <TablePage
      title={t('brood.roles.title')}
      icon={IconShield}
      description={t('brood.roles.description')}
      create={
        administrator
          ? {
              label: t('brood.roles.create'),
              sheet: (dialog) => (
                <RoleForm
                  {...dialog}
                  after={roles.refresh}
                />
              ),
            }
          : undefined
      }
      resource={roles}
      rowKey={(row) => row.id}
      rowName={(row) => row.name}
      columns={columns}
      panel={panel}
      defaultSort={SORT}
      empty={t('brood.roles.empty')}
    >
      {administrator &&
        edit.mount((dialog, role) => (
          <RoleForm {...dialog} role={role} after={roles.refresh} />
        ))}
    </TablePage>
  );
}
