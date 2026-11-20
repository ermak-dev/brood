import { useCallback, useState } from 'react';
import { Link, useSearchParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { Input } from '@/components/ui/Input';
import { Select, type SelectOption } from '@/components/ui/Select';
import { accessApi } from '@/services/brood/access';
import { agentsApi, type Agent, type AgentAction, type AgentRole } from '@/services/brood/agents';
import { useAdministrator, useResource } from './hooks';
import { agentsOf, swarmsOf } from './routes';
import { Field, FormDialog } from './table/dialogs';
import { useDialog, useForm } from './table/formState';
import { DASH, Truncate } from './table/format';
import { TablePage, type Column, type RowAction } from './table/TablePage';
import { StatusBadge, type BadgeTone } from './ui';
import styles from './table/table.module.scss';

/** How often the agents are read while the tab is visible. */
const POLL_MS = 4000;

/** Phases in the order of the default sort: what needs a look comes first. */
const PHASES = ['error', 'provisioning', 'stopping', 'deleting', 'running', 'stopped'];

const SORT = { column: 'state', desc: false };

const phaseTone = (phase: string): BadgeTone =>
  phase === 'error'
    ? 'problem'
    : phase === 'provisioning' || phase === 'stopping' || phase === 'deleting'
      ? 'progress'
      : 'neutral';

const phaseRank = (phase: string) =>
  (PHASES.indexOf(phase) + PHASES.length + 1) % (PHASES.length + 1);

const placement = (agent: Agent) =>
  [agent.pool, agent.ctid !== null ? `CT ${agent.ctid}` : ''].filter(Boolean).join(' · ');

const agentKey = (agent: Agent) => `${agent.swarmId}/${agent.name}`;

function CreateAgent({
  open,
  close,
  after,
  swarms,
  initialSwarm,
}: {
  open: boolean;
  close: () => void;
  after: () => unknown;
  swarms: SelectOption[];
  initialSwarm: string;
}) {
  const { t } = useTranslation();
  const administrator = useAdministrator();
  const [chosenSwarm, setChosenSwarm] = useState(initialSwarm);
  const swarm = swarms.some((option) => option.value === chosenSwarm)
    ? chosenSwarm
    : (swarms[0]?.value ?? '');
  // The profiles are those of the chosen swarm: they load when it changes.
  const profiles = useResource(
    useCallback(() => (swarm ? agentsApi.profiles(swarm) : Promise.resolve([])), [swarm])
  );
  const [chosenProfile, setChosenProfile] = useState('');
  const [role, setRole] = useState<AgentRole>('worker');
  const profileOptions = (profiles.data ?? []).map((name) => ({ value: name, label: name }));
  const profile = profileOptions.some((option) => option.value === chosenProfile)
    ? chosenProfile
    : (profileOptions[0]?.value ?? '');
  const form = useForm({ name: '', origin: '' }, ({ name, origin }) => ({
    name: name.trim() ? undefined : t('brood.form.required'),
    origin:
      !origin.trim() || /^\S+$/.test(origin.trim()) ? undefined : t('brood.agents.origin_invalid'),
  }));
  const name = form.values.name.trim();
  const profileProblem = profiles.error
    ? profiles.error
    : profiles.data?.length === 0 && swarm
      ? t('brood.agents.no_profiles')
      : undefined;
  return (
    <FormDialog
      open={open}
      close={close}
      title={t('brood.agents.create')}
      submitLabel={t('brood.form.create')}
      valid={form.valid && swarm !== '' && profile !== ''}
      submit={() =>
        agentsApi.create(swarm, { name, profile, role, origin: form.values.origin.trim() })
      }
      success={t('brood.agents.created', { name })}
      after={after}
    >
      <Input
        label={t('brood.name')}
        required
        autoComplete="off"
        autoCapitalize="off"
        spellCheck={false}
        {...form.field('name')}
      />
      <Field
        label={t('brood.agents.swarm')}
        required
        error={swarms.length === 0 ? t('brood.agents.no_swarms') : undefined}
      >
        {(id) => (
          <Select
            id={id}
            value={swarm}
            options={swarms}
            disabled={swarms.length === 0}
            onChange={setChosenSwarm}
          />
        )}
      </Field>
      <Field label={t('brood.agents.profile')} required error={profileProblem}>
        {(id) => (
          <Select
            id={id}
            value={profile}
            options={profileOptions}
            placeholder={profiles.loading && swarm ? t('brood.table.loading') : DASH}
            disabled={profileOptions.length === 0}
            onChange={setChosenProfile}
          />
        )}
      </Field>
      {administrator && (
        <Field label={t('brood.agents.role')} hint={t('brood.agents.role_hint')}>
          {(id) => (
            <Select
              id={id}
              value={role}
              options={[
                { value: 'worker', label: t('brood.agents.roles.worker') },
                { value: 'master', label: t('brood.agents.roles.master') },
              ]}
              onChange={(value) => setRole(value === 'master' ? 'master' : 'worker')}
            />
          )}
        </Field>
      )}
      <Input
        label={t('brood.agents.origin')}
        hint={t('brood.agents.origin_hint')}
        autoComplete="off"
        autoCapitalize="off"
        spellCheck={false}
        {...form.field('origin')}
      />
    </FormDialog>
  );
}

export function AgentsPage() {
  const { t } = useTranslation();
  const [params] = useSearchParams();
  const agents = useResource(agentsApi.list, POLL_MS);
  const swarms = useResource(accessApi.swarms);
  const create = useDialog();
  const project = params.get('project') ?? '';

  const swarmRows = swarms.data ?? [];
  const projectOptions = [
    ...new Map(swarmRows.map((swarm) => [swarm.project_id, swarm.project_name])),
  ].map(([value, label]) => ({ value, label }));
  const swarmLabel = (name: string, projectName: string) => `${name} (${projectName})`;
  const swarmOptions = swarmRows.map((swarm) => ({
    value: swarm.id,
    label: swarmLabel(swarm.name, swarm.project_name),
    project: swarm.project_id,
  }));

  const phaseLabel = (phase: string) =>
    PHASES.includes(phase) ? t(`brood.agents.phases.${phase}`) : phase || DASH;
  const roleLabel = (role: string) =>
    role === 'worker' || role === 'master' ? t(`brood.agents.roles.${role}`) : role || DASH;

  const columns: Column<Agent>[] = [
    {
      id: 'name',
      header: t('brood.name'),
      cell: (row) => <span className={styles.name}>{row.name}</span>,
      text: (row) => row.name,
      sort: (row) => row.name,
    },
    {
      id: 'swarm',
      header: t('brood.agents.swarm'),
      cell: (row) => <Link to={agentsOf(row.swarmId)}>{row.swarmName}</Link>,
      text: (row) => row.swarmName,
      sort: (row) => row.swarmName,
    },
    {
      id: 'project',
      header: t('brood.swarms.project'),
      cell: (row) => <Link to={swarmsOf(row.projectId)}>{row.projectName}</Link>,
      text: (row) => row.projectName,
      sort: (row) => row.projectName,
    },
    {
      id: 'role',
      header: t('brood.agents.role'),
      cell: (row) => roleLabel(row.role),
      text: (row) => roleLabel(row.role),
    },
    {
      id: 'state',
      header: t('brood.agents.state'),
      cell: (row) => <StatusBadge tone={phaseTone(row.phase)}>{phaseLabel(row.phase)}</StatusBadge>,
      text: (row) => phaseLabel(row.phase),
      sort: (row) => phaseRank(row.phase),
    },
    {
      id: 'placement',
      header: t('brood.agents.placement'),
      cell: (row) => <Truncate mono text={placement(row)} />,
      text: placement,
    },
    // The column exists only while some agent reports an error.
    ...((agents.data ?? []).some((row) => row.error)
      ? [
          {
            id: 'error',
            header: t('brood.agents.error'),
            cell: (row: Agent) => (row.error ? <Truncate tone="problem" text={row.error} /> : null),
            text: (row: Agent) => row.error,
          },
        ]
      : []),
  ];

  /** What the phase leaves to offer; the server decides for real. */
  const actions = (row: Agent): RowAction[] => {
    const act = (action: AgentAction) => () => agentsApi.act(row.swarmId, row.name, action);
    const deleting = row.phase === 'deleting' ? t('brood.agents.is_deleting') : undefined;
    const startable = row.phase === 'stopped' || row.phase === 'error';
    return [
      {
        id: 'terminal',
        label: t('brood.agents.terminal'),
        inline: true,
        href: agentsApi.terminal(row.swarmId, row.name),
        disabled: row.phase === 'running' ? undefined : t('brood.agents.terminal_not_running'),
      },
      // Start and Stop exclude each other: the row shows the one that applies.
      startable
        ? { id: 'resume', label: t('brood.agents.start'), inline: true, run: act('resume') }
        : {
            id: 'stop',
            label: t('brood.agents.stop'),
            inline: true,
            run: act('stop'),
            disabled:
              row.phase === 'running' || row.phase === 'provisioning'
                ? undefined
                : t('brood.agents.not_stoppable', { state: phaseLabel(row.phase) }),
          },
      {
        id: 'reset',
        label: t('brood.agents.reset'),
        danger: true,
        disabled: deleting,
        confirm: t('brood.agents.confirm_reset', { name: row.name }),
        run: act('reset'),
      },
      {
        id: 'delete',
        label: t('common.delete'),
        danger: true,
        disabled: deleting,
        confirm: t('brood.agents.confirm_delete', { name: row.name }),
        run: () => agentsApi.remove(row.swarmId, row.name),
      },
    ];
  };

  return (
    <TablePage
      title={t('brood.agents.title')}
      description={t('brood.agents.description')}
      primary={{ label: t('brood.agents.create'), onClick: () => create.show(null) }}
      resource={agents}
      rowKey={agentKey}
      rowName={(row) => row.name}
      columns={columns}
      filters={[
        {
          param: 'project',
          label: t('brood.swarms.project'),
          all: t('brood.swarms.all_projects'),
          options: projectOptions,
          value: (row) => row.projectId,
        },
        {
          param: 'swarm',
          label: t('brood.agents.swarm'),
          all: t('brood.agents.all_swarms'),
          // A chosen project narrows the swarms on offer.
          options: swarmOptions.filter(
            (option) =>
              !project || option.project === project || option.value === params.get('swarm')
          ),
          value: (row) => row.swarmId,
        },
      ]}
      defaultSort={SORT}
      empty={t('brood.agents.empty')}
      actions={actions}
    >
      {create.mount((dialog) => (
        <CreateAgent
          {...dialog}
          after={agents.refresh}
          swarms={swarmOptions}
          initialSwarm={params.get('swarm') ?? ''}
        />
      ))}
    </TablePage>
  );
}
