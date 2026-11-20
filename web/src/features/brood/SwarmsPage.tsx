import { useState } from 'react';
import { Link, useSearchParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { IconPlus, IconSidebarSwarms, IconTrash2 } from '@/components/ui/icons';
import { Input } from '@/components/ui/Input';
import { Select } from '@/components/ui/Select';
import { accessApi, type Project, type Swarm } from '@/services/brood/access';
import { useGuide } from './guide';
import { useResource } from './hooks';
import { IconArrowRight } from './icons';
import { agentsOf, createIn, swarmsOf } from './routes';
import { Field, FormSheet } from './table/dialogs';
import { PanelSection, type PanelSpec } from './table/EntityPanel';
import { useForm } from './table/formState';
import { RelativeTime } from './table/format';
import { TablePage, type Column } from './table/TablePage';
import { Commands, Copyable } from './ui';
import broodStyles from './brood.module.scss';
import styles from './table/table.module.scss';

const SORT = { column: 'name', desc: false };

function CreateSwarm({
  open,
  close,
  after,
  projects,
  swarms,
  initialProject,
}: {
  open: boolean;
  close: () => void;
  after: () => unknown;
  projects: Project[];
  swarms: Swarm[];
  /** The project of the page's filter; '' when none. */
  initialProject: string;
}) {
  const { t } = useTranslation();
  const has = (id: string) => projects.some((project) => project.id === id);
  const names = (project: string) =>
    swarms.filter((swarm) => swarm.project_id === project).map((swarm) => swarm.name);
  /** `main` while the project has no swarm named so. */
  const defaultName = (project: string) =>
    project && !names(project).includes('main') ? 'main' : '';
  // The filter of the page, or the only project there is; otherwise the user
  // chooses. Derived, not stored: the lists may arrive after the sheet opens.
  const [chosen, setChosen] = useState<string | null>(null);
  const preferred = has(initialProject)
    ? initialProject
    : projects.length === 1
      ? projects[0].id
      : '';
  const project = chosen !== null && has(chosen) ? chosen : preferred;
  const [checked, setChecked] = useState(false);
  // Until somebody types a name, it is the default of the chosen project.
  const [typed, setTyped] = useState(false);
  const form = useForm({ name: '' }, (values) => {
    const value = (typed ? values.name : defaultName(project)).trim();
    return {
      name: !value
        ? t('brood.form.required')
        : names(project).includes(value)
          ? t('brood.swarms.name_taken')
          : undefined,
    };
  });
  const shownName = typed ? form.values.name : defaultName(project);
  const name = shownName.trim();
  const projectProblem =
    projects.length === 0
      ? t('brood.swarms.no_projects')
      : checked && !project
        ? t('brood.swarms.choose_project')
        : undefined;
  return (
    <FormSheet
      open={open}
      close={close}
      title={t('brood.swarms.create')}
      icon={IconSidebarSwarms}
      description={t('brood.swarms.create_hint')}
      submitLabel={t('brood.swarms.create')}
      check={() => {
        setChecked(true);
        return form.check() && project !== '';
      }}
      dirty={typed && form.dirty}
      submit={() => accessApi.createSwarm(project, name)}
      success={t('brood.swarms.created', { name })}
      after={after}
    >
      <Input
        label={t('brood.name')}
        hint={t('brood.swarms.name_hint')}
        placeholder="main"
        required
        autoComplete="off"
        autoCapitalize="off"
        spellCheck={false}
        {...form.field('name')}
        value={shownName}
        onChange={(event) => {
          setTyped(true);
          form.set('name', event.target.value);
        }}
      />
      <Field
        label={t('brood.swarms.project')}
        required
        hint={t('brood.swarms.project_hint')}
        error={projectProblem}
      >
        {(id) => (
          <Select
            id={id}
            value={project}
            options={projects.map(({ id: value, name: label }) => ({ value, label }))}
            placeholder={t('brood.swarms.choose_project')}
            disabled={projects.length === 0}
            ariaInvalid={Boolean(projectProblem)}
            onChange={setChosen}
          />
        )}
      </Field>
    </FormSheet>
  );
}

export function SwarmsPage() {
  const { t, i18n } = useTranslation();
  const [params] = useSearchParams();
  const swarms = useResource(accessApi.swarms);
  const projects = useResource(accessApi.projects);
  const guide = useGuide(projects.data?.length === 0 ? 'project' : 'swarm');
  const projectOptions = (projects.data ?? []).map(({ id, name }) => ({ value: id, label: name }));

  const columns: Column<Swarm>[] = [
    {
      id: 'name',
      header: t('brood.name'),
      primary: true,
      cell: (row) => row.name,
      text: (row) => row.name,
      sort: (row) => row.name,
    },
    {
      id: 'project',
      header: t('brood.swarms.project'),
      cell: (row) => <Link to={swarmsOf(row.project_id)}>{row.project_name}</Link>,
      text: (row) => row.project_name,
      sort: (row) => row.project_name,
    },
    {
      id: 'owner',
      header: t('brood.swarms.owner'),
      cell: (row) => row.owner_username,
      text: (row) => row.owner_username,
      sort: (row) => row.owner_username,
    },
    {
      id: 'agents',
      header: t('brood.agents.title'),
      numeric: true,
      cell: (row) => <Link to={agentsOf(row.id, row.project_id)}>{row.agentCount}</Link>,
      sort: (row) => row.agentCount,
    },
    {
      id: 'created',
      header: t('brood.swarms.created_at'),
      cell: (row) => <RelativeTime value={row.created_at} />,
      sort: (row) => row.created_at,
    },
  ];

  const panel = (row: Swarm): PanelSpec => ({
    icon: IconSidebarSwarms,
    title: row.name,
    status: (
      <p className={styles.statusLine}>
        <span>
          {t('brood.swarms.project')}
          <Link to={swarmsOf(row.project_id)}>{row.project_name}</Link>
        </span>
        <span>
          {t('brood.swarms.owner')}
          <b>{row.owner_username}</b>
        </span>
        <span>
          <b>{t('brood.swarms.agent_count', { count: row.agentCount })}</b>
        </span>
        <span>
          {t('brood.swarms.created_at')}
          <b>
            <RelativeTime value={row.created_at} />
          </b>
        </span>
      </p>
    ),
    groups: [
      {
        id: 'work',
        label: t('brood.panel.actions'),
        actions: [
          {
            id: 'agents',
            label: t('brood.swarms.open_agents'),
            icon: IconArrowRight,
            description: t('brood.swarms.open_agents_hint'),
            tone: 'primary',
            to: agentsOf(row.id, row.project_id),
          },
          {
            id: 'create_agent',
            label: t('brood.agents.create'),
            icon: IconPlus,
            description: t('brood.swarms.create_agent_hint'),
            to: createIn('/agents', { project: row.project_id, swarm: row.id }),
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
            label: t('brood.swarms.delete'),
            icon: IconTrash2,
            description: t('brood.swarms.delete_hint'),
            tone: 'danger',
            blocked:
              row.agentCount > 0
                ? t('brood.swarms.has_agents', { count: row.agentCount })
                : undefined,
            confirm: t('brood.swarms.confirm_delete', { name: row.name }),
            run: () => accessApi.deleteSwarm(row.id),
          },
        ],
      },
    ],
    details: (
      <>
        <PanelSection title={t('brood.panel.details')}>
          <dl className={broodStyles.details}>
            <dt>{t('brood.name')}</dt>
            <dd>
              <Copyable value={row.name} />
            </dd>
            <dt>{t('brood.swarms.id')}</dt>
            <dd>
              <Copyable value={row.id} />
            </dd>
            <dt>{t('brood.swarms.created_at')}</dt>
            <dd>{new Date(row.created_at).toLocaleString(i18n.resolvedLanguage)}</dd>
          </dl>
        </PanelSection>
        <PanelSection title={t('brood.panel.commands')}>
          <Commands
            items={[
              { label: t('brood.swarms.command_scope'), command: `export BROOD_SWARM=${row.id}` },
              { label: t('brood.agents.command_list'), command: `bro agents --swarm ${row.id}` },
              {
                label: t('brood.swarms.command_add'),
                command: `bro add agent-1 --swarm ${row.id}`,
              },
            ]}
          />
        </PanelSection>
      </>
    ),
  });

  return (
    <TablePage
      title={t('brood.swarms.title')}
      icon={IconSidebarSwarms}
      description={t('brood.swarms.description')}
      create={{
        label: t('brood.swarms.create'),
        sheet: (dialog) => (
          <CreateSwarm
            {...dialog}
            after={swarms.refresh}
            projects={projects.data ?? []}
            swarms={swarms.data ?? []}
            initialProject={params.get('project') ?? ''}
          />
        ),
      }}
      resource={swarms}
      rowKey={(row) => row.id}
      rowName={(row) => row.name}
      columns={columns}
      filters={[
        {
          param: 'project',
          label: t('brood.swarms.project'),
          all: t('brood.swarms.all_projects'),
          options: projectOptions,
          value: (row) => row.project_id,
        },
      ]}
      panel={panel}
      defaultSort={SORT}
      empty={t('brood.swarms.empty')}
      guide={guide}
    />
  );
}
