import { useCallback, useState } from 'react';
import { Link, useSearchParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { Input } from '@/components/ui/Input';
import { Select } from '@/components/ui/Select';
import { IconPencil, IconShield, IconTrash2 } from '@/components/ui/icons';
import { accessApi, type Project } from '@/services/brood/access';
import {
  DELEGATED_ROLE_PERMISSIONS,
  ROLE_PERMISSIONS,
  rolesApi,
  type Role,
} from '@/services/brood/roles';
import { useAdministrator, useResource } from './hooks';
import { panelOf } from './routes';
import { Field, FormSection, FormSheet } from './table/dialogs';
import { PanelSection, type PanelSpec } from './table/EntityPanel';
import { useDialog, useForm } from './table/formState';
import { TablePage, type Column } from './table/TablePage';
import { StatusBadge } from './ui';
import styles from './table/table.module.scss';

type RoleRow = Role & { project: Project };
const SORT = { column: 'name', desc: false };

function RoleForm({
  open,
  close,
  after,
  projects,
  initialProject = '',
  role,
}: {
  open: boolean;
  close: () => void;
  after: () => unknown;
  projects: Project[];
  initialProject?: string;
  role?: RoleRow;
}) {
  const { t } = useTranslation();
  const [permissions, setPermissions] = useState(role?.permissions ?? []);
  const form = useForm(
    {
      name: role?.name ?? '',
      prompt: role?.prompt ?? '',
      project: role?.project.id ?? initialProject,
    },
    (values) => ({
      name: values.name.trim() ? undefined : t('brood.form.required'),
      project: projects.some(({ id }) => id === values.project)
        ? undefined
        : t('brood.form.required'),
    })
  );
  const projectId = form.values.project;
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
        return role ? rolesApi.update(projectId, role.id, body) : rolesApi.create(projectId, body);
      }}
      success={t(role ? 'brood.roles.saved' : 'brood.roles.created', {
        name: form.values.name.trim(),
      })}
      after={after}
    >
      {!role && (
        <Field
          label={t('brood.swarms.project')}
          required
          hint={t('brood.roles.project_hint')}
          error={form.error('project')}
        >
          {(id) => (
            <Select
              id={id}
              value={projectId}
              options={projects.map((project) => ({ value: project.id, label: project.name }))}
              placeholder={t('brood.swarms.choose_project')}
              ariaInvalid={Boolean(form.error('project'))}
              onChange={(value) => form.set('project', value)}
            />
          )}
        </Field>
      )}
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
  const [params] = useSearchParams();
  const projects = useResource(accessApi.projects);
  const projectRows = projects.data;
  const roles = useResource(
    useCallback(async () => {
      const lists = await Promise.all(
        (projectRows ?? []).map(async (project) =>
          (await rolesApi.list(project.id)).map((role): RoleRow => ({ ...role, project }))
        )
      );
      return lists.flat();
    }, [projectRows])
  );
  const resource = {
    data: projectRows ? roles.data : null,
    loading: projects.loading || roles.loading,
    error: projects.error || roles.error,
    refresh: async () => {
      if (projects.data) await roles.refresh();
      else await projects.refresh();
    },
  };
  const edit = useDialog<RoleRow>();
  const projectFilter = params.get('project') ?? '';
  const initialProject = (projectRows ?? []).some(({ id }) => id === projectFilter)
    ? projectFilter
    : projectRows?.length === 1
      ? projectRows[0].id
      : '';
  const permissionLabel = (key: string) =>
    t(`brood.roles.permission.${key}`, { defaultValue: key, nsSeparator: false });
  const columns: Column<RoleRow>[] = [
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
    {
      id: 'project',
      header: t('brood.swarms.project'),
      cell: (row) => row.project.name,
      text: (row) => row.project.name,
      sort: (row) => row.project.name,
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
  const panel = (row: RoleRow): PanelSpec => ({
    icon: IconShield,
    title: row.name,
    badge: row.default ? (
      <StatusBadge tone="neutral">{t('brood.roles.default')}</StatusBadge>
    ) : null,
    status: (
      <p className={styles.statusLine}>
        <span>
          {t('brood.swarms.project')}
          <Link to={panelOf('/projects', row.project.id)}>{row.project.name}</Link>
        </span>
      </p>
    ),
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
                  project: row.project.name,
                }),
                run: () => rolesApi.remove(row.project.id, row.id),
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
        administrator && projectRows?.length
          ? {
              label: t('brood.roles.create'),
              sheet: (dialog) => (
                <RoleForm
                  {...dialog}
                  after={roles.refresh}
                  projects={projectRows}
                  initialProject={initialProject}
                />
              ),
            }
          : undefined
      }
      resource={resource}
      rowKey={(row) => `${row.project.id}/${row.id}`}
      rowName={(row) => row.name}
      columns={columns}
      filters={[
        {
          param: 'project',
          label: t('brood.swarms.project'),
          all: t('brood.swarms.all_projects'),
          options: (projectRows ?? []).map(({ id, name }) => ({ value: id, label: name })),
          value: (row) => row.project.id,
        },
      ]}
      panel={panel}
      defaultSort={SORT}
      empty={t('brood.roles.empty')}
    >
      {administrator &&
        edit.mount((dialog, role) => (
          <RoleForm {...dialog} role={role} projects={[role.project]} after={roles.refresh} />
        ))}
    </TablePage>
  );
}
