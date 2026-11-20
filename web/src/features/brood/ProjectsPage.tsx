import { useEffect, useRef, useState } from 'react';
import { Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { Input } from '@/components/ui/Input';
import {
  accessApi,
  LIMIT_FIELDS,
  type Project,
  type ProjectLimits,
  type User,
} from '@/services/brood/access';
import { useAdministrator, useResource } from './hooks';
import { swarmsOf } from './routes';
import { Field, FormDialog } from './table/dialogs';
import { useDialog, useForm } from './table/formState';
import { DASH, Pending, Truncate } from './table/format';
import { TablePage, type Column } from './table/TablePage';
import styles from './table/table.module.scss';

/** What the list endpoint does not carry; each part arrives on its own and
 * stays undefined until then (null when the server refused it). */
interface Details {
  repository?: string | null;
  members?: User[] | null;
  limits?: ProjectLimits | null;
}

type Row = Project & Details;

const GIB = 1073741824;
const SORT = { column: 'name', desc: false };

/** Reads repository, members and limits of every listed project in
 * parallel after the list; the table is usable meanwhile. A reload of the
 * list re-reads them and keeps the previous values on screen until then. */
function useProjectDetails(projects: Project[] | null): Record<string, Details> {
  const [details, setDetails] = useState<Record<string, Details>>({});
  useEffect(() => {
    if (!projects) return;
    let stale = false;
    const put = (id: string, part: Details) =>
      stale || setDetails((prev) => ({ ...prev, [id]: { ...prev[id], ...part } }));
    for (const { id } of projects) {
      accessApi.repository(id).then(
        (repository) => put(id, { repository }),
        () => put(id, { repository: null })
      );
      accessApi.members(id).then(
        (members) => put(id, { members }),
        () => put(id, { members: null })
      );
      accessApi.limits(id).then(
        (limits) => put(id, { limits }),
        () => put(id, { limits: null })
      );
    }
    return () => {
      stale = true;
    };
  }, [projects]);
  return details;
}

/** The server takes nonnegative decimal strings; empty means unlimited. */
const validLimit = (value: string) => /^(?:|0|[1-9][0-9]{0,17})$/.test(value.trim());

function CreateProject({
  open,
  close,
  after,
}: {
  open: boolean;
  close: () => void;
  after: () => unknown;
}) {
  const { t } = useTranslation();
  const form = useForm({ name: '' }, ({ name }) => ({
    name: name.trim() ? undefined : t('brood.form.required'),
  }));
  const name = form.values.name.trim();
  return (
    <FormDialog
      open={open}
      close={close}
      title={t('brood.projects.create')}
      submitLabel={t('brood.form.create')}
      valid={form.valid}
      submit={() => accessApi.createProject(name)}
      success={t('brood.projects.created', { name })}
      after={after}
    >
      <Input label={t('brood.name')} required autoComplete="off" {...form.field('name')} />
    </FormDialog>
  );
}

function EditProject({
  project,
  open,
  close,
  after,
}: {
  project: Row;
  open: boolean;
  close: () => void;
  after: () => unknown;
}) {
  const { t } = useTranslation();
  const initial = {
    members: (project.members ?? []).map((member) => member.username),
    limits: project.limits ?? null,
  };
  // What the server holds; it advances with every accepted call, so a retry
  // after a refusal sends only what is still missing.
  const saved = useRef<typeof initial | null>(null);
  const [members, setMembers] = useState(initial.members);
  const [username, setUsername] = useState('');
  const form = useForm(
    Object.fromEntries(
      LIMIT_FIELDS.map((field) => [field, initial.limits?.[field] ?? ''])
    ) as Record<keyof ProjectLimits, string>,
    (values) =>
      Object.fromEntries(
        LIMIT_FIELDS.map((field) => [
          field,
          validLimit(values[field]) ? undefined : t('brood.projects.limit_invalid'),
        ])
      )
  );
  const candidate = username.trim();
  const duplicate = members.includes(candidate);
  const add = () => {
    if (!candidate || duplicate) return;
    setMembers([...members, candidate]);
    setUsername('');
  };
  const submit = async () => {
    const server = (saved.current ??= initial);
    for (const name of members.filter((member) => !server.members.includes(member))) {
      await accessApi.addMember(project.id, name);
      server.members = [...server.members, name];
    }
    for (const name of server.members.filter((member) => !members.includes(member))) {
      await accessApi.removeMember(project.id, name);
      server.members = server.members.filter((member) => member !== name);
    }
    const limits = Object.fromEntries(
      LIMIT_FIELDS.map((field) => [field, form.values[field].trim() || null])
    ) as unknown as ProjectLimits;
    if (LIMIT_FIELDS.some((field) => limits[field] !== (server.limits?.[field] ?? null))) {
      await accessApi.setLimits(project.id, limits);
      server.limits = limits;
    }
  };
  return (
    <FormDialog
      open={open}
      close={close}
      title={t('brood.projects.edit_title', { name: project.name })}
      submitLabel={t('common.save')}
      valid={form.valid}
      submit={submit}
      success={t('brood.projects.saved', { name: project.name })}
      after={after}
      width={600}
    >
      <Field label={t('brood.projects.members')} hint={t('brood.projects.members_hint')}>
        {() =>
          members.length === 0 ? (
            <p className={styles.fieldHint}>{t('brood.projects.no_members')}</p>
          ) : (
            <ul className={styles.list}>
              {members.map((member) => (
                <li key={member}>
                  {member}
                  <Button
                    type="button"
                    variant="ghost"
                    size="sm"
                    aria-label={t('brood.table.action_named', {
                      action: t('brood.projects.remove_member'),
                      name: member,
                    })}
                    onClick={() => setMembers(members.filter((name) => name !== member))}
                  >
                    {t('brood.projects.remove_member')}
                  </Button>
                </li>
              ))}
            </ul>
          )
        }
      </Field>
      <div className={styles.addRow}>
        <Input
          aria-label={t('brood.projects.member_username')}
          placeholder={t('brood.projects.member_username')}
          value={username}
          autoComplete="off"
          autoCapitalize="off"
          spellCheck={false}
          error={duplicate ? t('brood.projects.member_duplicate') : undefined}
          onChange={(event) => setUsername(event.target.value)}
          onKeyDown={(event) => {
            // Enter in this field adds the member; it does not save the form.
            if (event.key !== 'Enter' || !candidate) return;
            event.preventDefault();
            add();
          }}
        />
        <Button type="button" variant="secondary" disabled={!candidate || duplicate} onClick={add}>
          {t('brood.projects.add_member')}
        </Button>
      </div>
      <Field label={t('brood.projects.limits')} hint={t('brood.projects.limits_hint')}>
        {() => (
          <div className={styles.grid2}>
            {LIMIT_FIELDS.map((field) => (
              <Input
                key={field}
                label={t(`brood.projects.limit.${field}`)}
                inputMode="numeric"
                autoComplete="off"
                placeholder={t('brood.projects.unlimited')}
                {...form.field(field)}
              />
            ))}
          </div>
        )}
      </Field>
    </FormDialog>
  );
}

export function ProjectsPage() {
  const { t } = useTranslation();
  const administrator = useAdministrator();
  const projects = useResource(accessApi.projects);
  const details = useProjectDetails(projects.data);
  const create = useDialog();
  const edit = useDialog<Row>();
  const rows = {
    ...projects,
    data: projects.data?.map((project): Row => ({ ...project, ...details[project.id] })) ?? null,
  };

  /** "4 swarms · 12 agents · 8 CPU · 16 GiB": only the limits that are set. */
  const limitSummary = (limits: ProjectLimits | null) => {
    if (!limits) return '';
    const part = (value: string | null, key: string, unit = 1) =>
      value === null
        ? []
        : [
            t(`brood.projects.limit_short.${key}`, {
              count: Math.round((Number(value) / unit) * 10) / 10,
            }),
          ];
    return [
      ...part(limits.max_swarms, 'swarms'),
      ...part(limits.max_actors, 'agents'),
      ...part(limits.max_cpu_millicores, 'cpu', 1000),
      ...part(limits.max_memory_bytes, 'memory', GIB),
    ].join(' · ');
  };

  const columns: Column<Row>[] = [
    {
      id: 'name',
      header: t('brood.name'),
      cell: (row) => <span className={styles.name}>{row.name}</span>,
      text: (row) => row.name,
      sort: (row) => row.name,
    },
    {
      id: 'repository',
      header: t('brood.projects.repository'),
      cell: (row) => (
        <Pending value={row.repository}>{(value) => <Truncate mono text={value ?? ''} />}</Pending>
      ),
      text: (row) => row.repository ?? '',
    },
    {
      id: 'swarms',
      header: t('brood.swarms.title'),
      numeric: true,
      cell: (row) => <Link to={swarmsOf(row.id)}>{row.swarmCount ?? 0}</Link>,
    },
    {
      id: 'members',
      header: t('brood.projects.members'),
      numeric: true,
      cell: (row) => (
        <Pending value={row.members}>
          {(members) =>
            members ? (
              <span title={members.map((member) => member.username).join(', ')}>
                {members.length}
              </span>
            ) : (
              DASH
            )
          }
        </Pending>
      ),
    },
    {
      id: 'limits',
      header: t('brood.projects.limits'),
      cell: (row) => (
        <Pending value={row.limits}>{(limits) => <Truncate text={limitSummary(limits)} />}</Pending>
      ),
    },
  ];

  return (
    <TablePage
      title={t('brood.projects.title')}
      description={t('brood.projects.description')}
      primary={
        administrator
          ? { label: t('brood.projects.create'), onClick: () => create.show(null) }
          : undefined
      }
      resource={rows}
      rowKey={(row) => row.id}
      rowName={(row) => row.name}
      columns={columns}
      defaultSort={SORT}
      empty={t(administrator ? 'brood.projects.empty' : 'brood.projects.empty_member')}
      actions={
        administrator
          ? (row) => [
              {
                id: 'edit',
                label: t('common.edit'),
                inline: true,
                disabled:
                  row.members === undefined || row.limits === undefined
                    ? t('brood.table.loading')
                    : undefined,
                open: () => edit.show(row),
              },
              {
                id: 'delete',
                label: t('common.delete'),
                danger: true,
                confirm: t('brood.projects.confirm_delete', { name: row.name }),
                run: () => accessApi.deleteProject(row.id),
              },
            ]
          : undefined
      }
    >
      {create.mount((dialog) => (
        <CreateProject {...dialog} after={projects.refresh} />
      ))}
      {edit.mount((dialog, project) => (
        <EditProject {...dialog} project={project} after={projects.refresh} />
      ))}
    </TablePage>
  );
}
