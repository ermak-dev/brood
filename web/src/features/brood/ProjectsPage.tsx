import { useCallback, useRef, useState } from 'react';
import { Link, useSearchParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import {
  IconAlertTriangle,
  IconCheckCircle2,
  IconPencil,
  IconPlus,
  IconSidebarProjects,
  IconTrash2,
} from '@/components/ui/icons';
import { Input } from '@/components/ui/Input';
import {
  accessApi,
  LIMIT_FIELDS,
  type Project,
  type ProjectImage,
  type ProjectLimits,
} from '@/services/brood/access';
import { useGuide } from './guide';
import { useAdministrator, useResource } from './hooks';
import { IconArrowRight, IconBox } from './icons';
import { createIn, swarmsOf } from './routes';
import { FormSection, FormSheet } from './table/dialogs';
import { PanelSection, type PanelAction, type PanelSpec } from './table/EntityPanel';
import { useDialog, useForm } from './table/formState';
import { DASH, RelativeTime, Truncate } from './table/format';
import { TablePage, type Column } from './table/TablePage';
import { GIB, scaled } from './table/units';
import { Commands, Copyable, StatusBadge, type BadgeTone, type Icon } from './ui';
import broodStyles from './brood.module.scss';
import styles from './table/table.module.scss';

const SORT = { column: 'name', desc: false };
/** How often the images of the open project are read: a build is slow. */
const IMAGES_POLL_MS = 5000;

type LimitField = keyof ProjectLimits;

/** What the form shows per unit of the API: CPU in cores, memory in GiB. */
const LIMIT_UNIT: Record<LimitField, number> = {
  max_swarms: 1,
  max_actors: 1,
  max_cpu_millicores: 1000,
  max_memory_bytes: GIB,
};

/** What a limit starts from when "unlimited" is switched off, in shown units. */
const LIMIT_DEFAULT: Record<LimitField, string> = {
  max_swarms: '4',
  max_actors: '12',
  max_cpu_millicores: '16',
  max_memory_bytes: '32',
};

/** Whole numbers for counts; cores and GiB take up to three decimals. */
const validLimit = (field: LimitField, value: string) =>
  (LIMIT_UNIT[field] === 1 ? /^(?:|0|[1-9][0-9]{0,8})$/ : /^(?:|[0-9]{1,9}(?:\.[0-9]{1,3})?)$/).test(
    value.trim()
  );

const IMAGE_LOOK: Record<string, { tone: BadgeTone; icon?: Icon; busy?: boolean }> = {
  building: { tone: 'attention', busy: true },
  ready: { tone: 'progress', icon: IconCheckCircle2 },
  failed: { tone: 'problem', icon: IconAlertTriangle },
  retired: { tone: 'neutral', icon: IconBox },
};

function CreateProject({
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
  const form = useForm({ name: '' }, ({ name }) => ({
    name: !name.trim()
      ? t('brood.form.required')
      : taken.includes(name.trim())
        ? t('brood.projects.name_taken')
        : undefined,
  }));
  const name = form.values.name.trim();
  return (
    <FormSheet
      open={open}
      close={close}
      title={t('brood.projects.create')}
      icon={IconSidebarProjects}
      description={t('brood.projects.create_hint')}
      submitLabel={t('brood.projects.create')}
      check={form.check}
      dirty={form.dirty}
      submit={() => accessApi.createProject(name)}
      success={t('brood.projects.created', { name })}
      after={after}
    >
      <Input
        label={t('brood.name')}
        hint={t('brood.projects.name_hint')}
        placeholder="my-project"
        required
        autoComplete="off"
        autoCapitalize="off"
        spellCheck={false}
        {...form.field('name')}
      />
    </FormSheet>
  );
}

function EditProject({
  project,
  open,
  close,
  after,
}: {
  project: Project;
  open: boolean;
  close: () => void;
  after: () => unknown;
}) {
  const { t } = useTranslation();
  // Members are picked from the user list, never typed.
  const users = useResource(accessApi.users);
  // What the form started from, in its own units.
  const [shownLimits] = useState(
    () =>
      Object.fromEntries(
        LIMIT_FIELDS.map((field) => {
          const value = project.capacity[field];
          return [field, value === null ? '' : String(Number(value) / LIMIT_UNIT[field])];
        })
      ) as Record<LimitField, string>
  );
  // What the server holds; it advances with every accepted call, so a retry
  // after a refusal sends only what is still missing.
  const saved = useRef<{ members: string[]; limits: ProjectLimits } | null>(null);
  const [members, setMembers] = useState(project.members);
  const form = useForm(shownLimits, (values) =>
    Object.fromEntries(
      LIMIT_FIELDS.map((field) => [
        field,
        validLimit(field, values[field])
          ? undefined
          : t(
              LIMIT_UNIT[field] === 1
                ? 'brood.projects.limit_invalid'
                : 'brood.projects.limit_invalid_decimal'
            ),
      ])
    )
  );
  // Active users plus current members that are disabled or gone from the
  // list, so that they can still be removed.
  const listed = (users.data ?? []).filter((user) => !user.disabled);
  const choices = [
    ...listed.map((user) => ({ username: user.username, name: user.display_name })),
    ...members
      .filter((member) => !listed.some((user) => user.username === member))
      .map((member) => ({ username: member, name: '' })),
  ].sort((a, b) => a.username.localeCompare(b.username));
  const toggle = (name: string, on: boolean) =>
    setMembers(on ? [...members, name] : members.filter((member) => member !== name));
  /** A field nobody touched goes back exactly as the server gave it. */
  const wire = (field: LimitField): string | null => {
    const text = form.values[field].trim();
    if (text === shownLimits[field]) return project.capacity[field];
    return text === '' ? null : String(Math.round(Number(text) * LIMIT_UNIT[field]));
  };
  const submit = async () => {
    const server = (saved.current ??= { members: project.members, limits: project.capacity });
    for (const name of members.filter((member) => !server.members.includes(member))) {
      await accessApi.addMember(project.id, name);
      server.members = [...server.members, name];
    }
    for (const name of server.members.filter((member) => !members.includes(member))) {
      await accessApi.removeMember(project.id, name);
      server.members = server.members.filter((member) => member !== name);
    }
    const limits = Object.fromEntries(
      LIMIT_FIELDS.map((field) => [field, wire(field)])
    ) as unknown as ProjectLimits;
    if (LIMIT_FIELDS.some((field) => limits[field] !== server.limits[field])) {
      await accessApi.setLimits(project.id, limits);
      server.limits = limits;
    }
  };
  const membersChanged =
    members.length !== project.members.length ||
    members.some((member) => !project.members.includes(member));
  return (
    <FormSheet
      open={open}
      close={close}
      title={t('brood.projects.edit_title', { name: project.name })}
      icon={IconPencil}
      submitLabel={t('common.save')}
      check={form.check}
      dirty={form.dirty || membersChanged}
      submit={submit}
      success={t('brood.projects.saved', { name: project.name })}
      after={after}
    >
      <FormSection title={t('brood.projects.members')} hint={t('brood.projects.members_hint')}>
        {choices.length === 0 ? (
          <p className={styles.fieldHint}>
            {t(users.data ? 'brood.projects.no_users' : 'brood.projects.users_loading')}
          </p>
        ) : (
          <ul className={styles.checkList}>
            {choices.map((user) => (
              <li key={user.username}>
                <label>
                  <input
                    type="checkbox"
                    checked={members.includes(user.username)}
                    onChange={(event) => toggle(user.username, event.target.checked)}
                  />
                  <span>{user.username}</span>
                  {user.name && <span className={styles.checkNote}>{user.name}</span>}
                </label>
              </li>
            ))}
          </ul>
        )}
      </FormSection>
      <FormSection title={t('brood.projects.limits')} hint={t('brood.projects.limits_hint')}>
        <div className={styles.grid2}>
          {LIMIT_FIELDS.map((field) => {
            const unlimited = form.values[field].trim() === '';
            return (
              <div key={field} className={styles.limitField}>
                <Input
                  label={t(`brood.projects.limit.${field}`)}
                  hint={t(`brood.projects.limit_help.${field}`)}
                  inputMode={LIMIT_UNIT[field] === 1 ? 'numeric' : 'decimal'}
                  autoComplete="off"
                  disabled={unlimited}
                  placeholder={t('brood.projects.unlimited')}
                  {...form.field(field)}
                />
                <label className={styles.checkLine}>
                  <input
                    type="checkbox"
                    checked={unlimited}
                    onChange={(event) =>
                      form.set(field, event.target.checked ? '' : LIMIT_DEFAULT[field])
                    }
                  />
                  {t('brood.projects.unlimited')}
                </label>
              </div>
            );
          })}
        </div>
      </FormSection>
    </FormSheet>
  );
}

export function ProjectsPage() {
  const { t } = useTranslation();
  const administrator = useAdministrator();
  const [params] = useSearchParams();
  const projects = useResource(accessApi.projects);
  const edit = useDialog<Project>();
  const guide = useGuide('project');

  // Images of the project whose panel is open; none otherwise.
  const openId = params.get('open') ?? '';
  const images = useResource(
    useCallback(
      () => (openId ? accessApi.images(openId) : Promise.resolve<ProjectImage[]>([])),
      [openId]
    ),
    openId ? IMAGES_POLL_MS : null
  );

  const limitValue = (field: LimitField, value: string | null) =>
    value === null
      ? t('brood.projects.unlimited')
      : t(`brood.projects.limit_short.${field}`, { count: scaled(value, LIMIT_UNIT[field]) });
  /** "4 swarms · 12 agents · 8 CPU · 16 GiB": only the limits that are set. */
  const limitSummary = (limits: ProjectLimits) =>
    LIMIT_FIELDS.filter((field) => limits[field] !== null)
      .map((field) => limitValue(field, limits[field]))
      .join(' · ');

  const columns: Column<Project>[] = [
    {
      id: 'name',
      header: t('brood.name'),
      primary: true,
      cell: (row) => row.name,
      text: (row) => row.name,
      sort: (row) => row.name,
    },
    {
      id: 'repository',
      header: t('brood.projects.repository'),
      cell: (row) => <Truncate mono text={row.repository} />,
      text: (row) => row.repository,
    },
    {
      id: 'swarms',
      header: t('brood.swarms.title'),
      numeric: true,
      cell: (row) => <Link to={swarmsOf(row.id)}>{row.swarmCount}</Link>,
      sort: (row) => row.swarmCount,
    },
    {
      id: 'members',
      header: t('brood.projects.members'),
      numeric: true,
      cell: (row) => <span title={row.members.join(', ')}>{row.members.length}</span>,
      text: (row) => row.members.join(' '),
      sort: (row) => row.members.length,
    },
    {
      id: 'limits',
      header: t('brood.projects.limits'),
      cell: (row) => (
        <Truncate text={limitSummary(row.capacity) || t('brood.projects.unlimited')} />
      ),
    },
  ];

  const imageName = (image: ProjectImage) => image.snapshot.slice(0, 8) || image.id;
  const imageList = (project: Project) => {
    const items = openId === project.id ? images.data : null;
    if (images.error && !items) return <p className={styles.failure}>{images.error}</p>;
    if (!items) return <p className={styles.fieldHint}>{t('brood.table.loading')}</p>;
    if (items.length === 0)
      return <p className={styles.fieldHint}>{t('brood.projects.no_images')}</p>;
    return (
      <ul className={styles.items}>
        {items.map((image) => {
          const look = IMAGE_LOOK[image.state] ?? { tone: 'neutral' as const };
          return (
            <li key={image.id}>
              <p className={styles.statusLine}>
                <StatusBadge tone={look.tone} icon={look.icon} busy={look.busy}>
                  {t(`brood.projects.image_states.${image.state}`, image.state)}
                </StatusBadge>
                <span title={image.snapshot}>
                  <Copyable value={image.snapshot} shown={imageName(image)} />
                </span>
                <span>
                  {t('brood.agents.pool')}
                  <b>{image.pool || DASH}</b>
                </span>
                <span>
                  <RelativeTime value={image.created_at} />
                </span>
              </p>
              {image.error && <p className={styles.errorText}>{image.error}</p>}
              {image.log && (
                <details>
                  <summary>{t('brood.projects.image_log')}</summary>
                  <pre className={styles.log}>{image.log}</pre>
                </details>
              )}
            </li>
          );
        })}
      </ul>
    );
  };

  const panel = (row: Project): PanelSpec => {
    const own = openId === row.id ? (images.data ?? []) : [];
    const adminOnly = administrator ? undefined : t('brood.projects.admin_only');
    const deleteImages: PanelAction[] = own.map((image) => ({
      id: `image-${image.id}`,
      label: t('brood.projects.delete_image', { name: imageName(image) }),
      icon: IconBox,
      description: t('brood.projects.delete_image_hint', { pool: image.pool || DASH }),
      tone: 'danger',
      blocked: image.state === 'building' ? t('brood.projects.image_building') : undefined,
      confirm: t('brood.projects.confirm_delete_image', {
        name: imageName(image),
        project: row.name,
      }),
      run: async () => {
        await accessApi.deleteImage(row.id, image.id);
        await images.refresh();
      },
    }));
    return {
      icon: IconSidebarProjects,
      title: row.name,
      status: (
        <p className={styles.statusLine}>
          <span>
            <Link to={swarmsOf(row.id)}>
              {t('brood.projects.swarm_count', { count: row.swarmCount })}
            </Link>
          </span>
          <span>
            <b>{t('brood.projects.member_count', { count: row.members.length })}</b>
          </span>
          <span>
            {row.repository ? <Copyable value={row.repository} /> : t('brood.projects.no_repository')}
          </span>
        </p>
      ),
      groups: [
        {
          id: 'work',
          label: t('brood.panel.actions'),
          actions: [
            {
              id: 'swarms',
              label: t('brood.projects.open_swarms'),
              icon: IconArrowRight,
              description: t('brood.projects.open_swarms_hint'),
              tone: 'primary',
              to: swarmsOf(row.id),
            },
            {
              id: 'create_swarm',
              label: t('brood.swarms.create'),
              icon: IconPlus,
              description: t('brood.projects.create_swarm_hint'),
              to: createIn('/swarms', { project: row.id }),
            },
            {
              id: 'edit',
              label: t('brood.projects.edit'),
              icon: IconPencil,
              description: t('brood.projects.edit_hint'),
              blocked: adminOnly,
              onSelect: () => edit.show(row),
            },
          ],
        },
        {
          id: 'danger',
          label: t('brood.panel.danger'),
          danger: true,
          actions: [
            ...deleteImages,
            {
              id: 'delete',
              label: t('brood.projects.delete'),
              icon: IconTrash2,
              description: t('brood.projects.delete_hint'),
              tone: 'danger',
              blocked:
                adminOnly ??
                (row.swarmCount > 0
                  ? t('brood.projects.has_swarms', { count: row.swarmCount })
                  : own.length > 0
                    ? t('brood.projects.has_images', { count: own.length })
                    : undefined),
              confirm: t('brood.projects.confirm_delete', { name: row.name }),
              run: () => accessApi.deleteProject(row.id),
            },
          ],
        },
      ],
      details: (
        <>
          <PanelSection title={t('brood.panel.details')}>
            <dl className={broodStyles.details}>
              <dt>{t('brood.projects.id')}</dt>
              <dd>
                <Copyable value={row.id} />
              </dd>
              <dt>{t('brood.projects.members')}</dt>
              <dd>{row.members.join(', ') || t('brood.projects.no_members')}</dd>
              {LIMIT_FIELDS.map((field) => (
                <FragmentRow
                  key={field}
                  label={t(`brood.projects.limit.${field}`)}
                  value={
                    row.capacity[field] === null
                      ? t('brood.projects.unlimited')
                      : String(scaled(row.capacity[field] ?? '', LIMIT_UNIT[field]))
                  }
                />
              ))}
            </dl>
          </PanelSection>
          <PanelSection title={t('brood.projects.images')}>
            <p className={styles.fieldHint}>{t('brood.projects.images_hint')}</p>
            {imageList(row)}
          </PanelSection>
          <PanelSection title={t('brood.panel.commands')}>
            <Commands
              items={[
                { label: t('brood.projects.command_build'), command: 'bro build' },
                {
                  label: t('brood.projects.command_images'),
                  command: `bro image list --project ${row.name}`,
                },
              ]}
            />
          </PanelSection>
        </>
      ),
    };
  };

  return (
    <TablePage
      title={t('brood.projects.title')}
      icon={IconSidebarProjects}
      description={t('brood.projects.description')}
      create={
        administrator
          ? {
              label: t('brood.projects.create'),
              sheet: (dialog) => (
                <CreateProject
                  {...dialog}
                  after={projects.refresh}
                  taken={(projects.data ?? []).map((project) => project.name)}
                />
              ),
            }
          : undefined
      }
      resource={projects}
      rowKey={(row) => row.id}
      rowName={(row) => row.name}
      columns={columns}
      panel={panel}
      defaultSort={SORT}
      empty={t(administrator ? 'brood.projects.empty' : 'brood.projects.empty_member')}
      guide={guide}
    >
      {edit.mount((dialog, project) => (
        <EditProject {...dialog} project={project} after={projects.refresh} />
      ))}
    </TablePage>
  );
}

function FragmentRow({ label, value }: { label: string; value: string }) {
  return (
    <>
      <dt>{label}</dt>
      <dd>{value}</dd>
    </>
  );
}
