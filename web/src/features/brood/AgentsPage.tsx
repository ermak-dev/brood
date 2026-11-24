import { useCallback, useMemo, useState } from 'react';
import { Link, useSearchParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import {
  IconAlertTriangle,
  IconCheck,
  IconEye,
  IconSidebarAgents,
  IconTrash2,
} from '@/components/ui/icons';
import { Input } from '@/components/ui/Input';
import { Select } from '@/components/ui/Select';
import { accessApi, type Swarm } from '@/services/brood/access';
import { agentsApi, type Agent, type AgentAction } from '@/services/brood/agents';
import { DELEGATED_ROLE_PERMISSIONS, rolesApi } from '@/services/brood/roles';
import { useGuide } from './guide';
import { useAdministrator, useResource } from './hooks';
import { IconPlay, IconRotateCcw, IconSquare, IconTerminal } from './icons';
import { agentsOf, swarmsOf } from './routes';
import { Field, FormSheet } from './table/dialogs';
import { PanelSection, type PanelSpec } from './table/EntityPanel';
import { useForm } from './table/formState';
import { DASH, RelativeTime, Truncate } from './table/format';
import { SummaryChip, TablePage, type Column } from './table/TablePage';
import { GIB, scaled } from './table/units';
import { Commands, Copyable, StatusBadge, type BadgeTone, type Icon } from './ui';
import broodStyles from './brood.module.scss';
import styles from './table/table.module.scss';

/** How often the agents are read while the tab is visible. */
const POLL_MS = 4000;
/** How long a requested state stands in for the server's before it gives up. */
const REQUEST_MS = 45000;

/** Phases in the order of the default sort: what needs a look comes first.
 * `resetting` exists only here, between a reset request and its effect. */
const PHASES = [
  'error',
  'provisioning',
  'resetting',
  'stopping',
  'deleting',
  'running',
  'stopped',
] as const;

const PHASE_LOOK: Record<string, { tone: BadgeTone; icon?: Icon; busy?: boolean }> = {
  error: { tone: 'problem', icon: IconAlertTriangle },
  provisioning: { tone: 'attention', busy: true },
  resetting: { tone: 'attention', busy: true },
  stopping: { tone: 'attention', busy: true },
  deleting: { tone: 'neutral', icon: IconTrash2 },
  running: { tone: 'progress', icon: IconPlay },
  stopped: { tone: 'neutral', icon: IconSquare },
};

/** The verdicts on a running agent, in the order of the sort: who needs a
 * look comes first, who can take work last. */
const ACTIVITIES = [
  'error',
  'offline',
  'needs_action',
  'waiting',
  'idle',
  'unknown',
  'busy',
  'free',
] as const;

const ACTIVITY_LOOK: Record<string, { tone: BadgeTone; icon?: Icon; busy?: boolean }> = {
  error: { tone: 'problem', icon: IconAlertTriangle },
  offline: { tone: 'problem', icon: IconAlertTriangle },
  needs_action: { tone: 'attention' },
  waiting: { tone: 'attention' },
  idle: { tone: 'attention' },
  unknown: { tone: 'neutral' },
  busy: { tone: 'progress', busy: true },
  free: { tone: 'neutral', icon: IconCheck },
};

/** The four counts of the summary strip and the phase each filters by. */
const SUMMARY: { phase: string; tone: BadgeTone; icon: Icon }[] = [
  { phase: 'running', tone: 'progress', icon: IconPlay },
  { phase: 'provisioning', tone: 'attention', icon: IconRotateCcw },
  { phase: 'stopped', tone: 'neutral', icon: IconSquare },
  { phase: 'error', tone: 'problem', icon: IconAlertTriangle },
];

const SORT = { column: 'state', desc: false };
const NAME = /^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$/;

/** An agent as the page shows it: `requested` marks a state the user asked
 * for here that the server has not reported yet. */
type Row = Agent & { requested?: boolean };

type Verb = AgentAction | 'delete';
/** What the row shows from the request until the server reports a change. */
const REQUESTED_PHASE: Record<Verb, string> = {
  resume: 'provisioning',
  stop: 'stopping',
  reset: 'resetting',
  delete: 'deleting',
};

const phaseRank = (phase: string) => {
  const index = (PHASES as readonly string[]).indexOf(phase);
  return index < 0 ? PHASES.length : index;
};
const activityRank = (activity: string) => {
  const index = (ACTIVITIES as readonly string[]).indexOf(activity);
  return index < 0 ? ACTIVITIES.length : index;
};
/** The phase the state filter and the summary count a row under. */
const filterPhase = (row: Row) => (row.phase === 'resetting' ? 'provisioning' : row.phase);

const placement = (agent: Agent) =>
  [agent.pool, agent.ctid !== null ? `CT ${agent.ctid}` : ''].filter(Boolean).join(' · ');

const agentKey = (agent: Agent) => `${agent.swarmId}/${agent.name}`;

/** The first of agent-1, agent-2, ... that no agent of the swarm has. */
function nextName(agents: Agent[], swarm: string) {
  const taken = new Set(agents.filter((agent) => agent.swarmId === swarm).map(({ name }) => name));
  let number = 1;
  while (taken.has(`agent-${number}`)) number += 1;
  return `agent-${number}`;
}

function CreateAgent({
  open,
  close,
  after,
  swarms,
  agents,
  initialSwarm,
}: {
  open: boolean;
  close: () => void;
  after: () => unknown;
  swarms: Swarm[];
  agents: Agent[];
  /** The swarm of the page's filter; '' when none. */
  initialSwarm: string;
}) {
  const { t } = useTranslation();
  const administrator = useAdministrator();
  const swarmOptions = swarms.map((swarm) => ({
    value: swarm.id,
    label: `${swarm.name} (${swarm.project_name})`,
  }));
  const has = (id: string) => swarms.some((swarm) => swarm.id === id);
  // The filter of the page, or the only swarm there is; otherwise the user
  // chooses. Derived, not stored: the lists may arrive after the sheet opens.
  const [chosenSwarm, setChosenSwarm] = useState<string | null>(null);
  const preferred = has(initialSwarm) ? initialSwarm : swarms.length === 1 ? swarms[0].id : '';
  const swarm = chosenSwarm !== null && has(chosenSwarm) ? chosenSwarm : preferred;
  // The profiles are those of the chosen swarm: they load when it changes.
  const profiles = useResource(
    useCallback(() => (swarm ? agentsApi.profiles(swarm) : Promise.resolve([])), [swarm])
  );
  const [chosenProfile, setChosenProfile] = useState('');
  const roles = useResource(rolesApi.list);
  const assignableRoles = (roles.data ?? []).filter(
    (role) => administrator || !role.permissions.some((permission) => DELEGATED_ROLE_PERMISSIONS.has(permission))
  );
  const [chosenRole, setChosenRole] = useState('');
  const defaultRole = assignableRoles.find((role) => role.default)?.id ?? '';
  const roleId = assignableRoles.some(({ id }) => id === chosenRole) ? chosenRole : defaultRole;
  const roleOptions = assignableRoles.map(({ id, name }) => ({ value: id, label: name }));
  const [checked, setChecked] = useState(false);
  const profileOptions = (profiles.data ?? []).map((name) => ({ value: name, label: name }));
  const profile = profileOptions.some((option) => option.value === chosenProfile)
    ? chosenProfile
    : profileOptions.length === 1
      ? profileOptions[0].value
      : '';
  // Until somebody types a name, it is the next free one of the chosen swarm.
  const [typed, setTyped] = useState(false);
  const freeName = nextName(agents, swarm);
  const form = useForm({ name: '', origin: '' }, (values) => {
    const value = (typed ? values.name : freeName).trim();
    return {
      name: !value
        ? t('brood.form.required')
        : !NAME.test(value) || value.length > 63
          ? t('brood.agents.name_invalid')
          : value === 'operator'
            ? t('brood.agents.name_reserved')
            : agents.some((agent) => agent.swarmId === swarm && agent.name === value)
              ? t('brood.agents.name_taken')
              : undefined,
      origin:
        !values.origin.trim() || /^\S+$/.test(values.origin.trim())
          ? undefined
          : t('brood.agents.origin_invalid'),
    };
  });
  const shownName = typed ? form.values.name : freeName;
  const name = shownName.trim();
  const swarmProblem =
    swarms.length === 0
      ? t('brood.agents.no_swarms')
      : checked && !swarm
        ? t('brood.agents.choose_swarm')
        : undefined;
  const profileProblem = !swarm
    ? undefined
    : profiles.error
      ? profiles.error
      : profiles.data?.length === 0
        ? t('brood.agents.no_profiles')
        : checked && !profile
          ? t('brood.agents.choose_profile')
          : undefined;
  const roleProblem =
    roles.loading
      ? undefined
      : roles.error ||
        (!roleOptions.length
          ? t('brood.agents.no_roles')
          : checked && !roleId
            ? t('brood.agents.choose_role')
            : undefined);
  return (
    <FormSheet
      open={open}
      close={close}
      title={t('brood.agents.create')}
      icon={IconSidebarAgents}
      description={t('brood.agents.create_hint')}
      submitLabel={t('brood.agents.create')}
      check={() => {
        setChecked(true);
        return form.check() && swarm !== '' && profile !== '' && roleId !== '' && !roles.error;
      }}
      dirty={(typed && form.dirty) || form.values.origin !== '' || roleId !== defaultRole}
      submit={() =>
        agentsApi.create(swarm, { name, profile, roleId, origin: form.values.origin.trim() })
      }
      success={t('brood.agents.created', { name })}
      after={after}
    >
      <Input
        label={t('brood.name')}
        hint={t('brood.agents.name_hint')}
        placeholder="agent-1"
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
        label={t('brood.agents.swarm')}
        required
        hint={t('brood.agents.swarm_hint')}
        error={swarmProblem}
      >
        {(id) => (
          <Select
            id={id}
            value={swarm}
            options={swarmOptions}
            placeholder={t('brood.agents.choose_swarm')}
            disabled={swarms.length === 0}
            ariaInvalid={Boolean(swarmProblem)}
            onChange={setChosenSwarm}
          />
        )}
      </Field>
      <Field
        label={t('brood.agents.profile')}
        required
        hint={t('brood.agents.profile_hint')}
        error={profileProblem}
      >
        {(id) => (
          <Select
            id={id}
            value={profile}
            options={profileOptions}
            placeholder={
              !swarm
                ? t('brood.agents.profile_needs_swarm')
                : profiles.loading
                  ? t('brood.table.loading')
                  : t('brood.agents.choose_profile')
            }
            disabled={profileOptions.length === 0}
            ariaInvalid={Boolean(profileProblem)}
            onChange={setChosenProfile}
          />
        )}
      </Field>
      <Field
        label={t('brood.agents.role')}
        required
        hint={t('brood.agents.role_hint')}
        error={roleProblem}
      >
        {(id) => (
          <Select
            id={id}
            value={roleId}
            options={roleOptions}
            placeholder={
              roles.loading ? t('brood.table.loading') : t('brood.agents.choose_role')
            }
            disabled={roleOptions.length === 0 || Boolean(roles.error)}
            ariaInvalid={Boolean(roleProblem)}
            onChange={setChosenRole}
          />
        )}
      </Field>
      <Input
        label={t('brood.agents.origin')}
        hint={t('brood.agents.origin_hint')}
        placeholder="https://git.example.com/team/project.git"
        autoComplete="off"
        autoCapitalize="off"
        spellCheck={false}
        {...form.field('origin')}
      />
    </FormSheet>
  );
}

export function AgentsPage() {
  const { t, i18n } = useTranslation();
  const [params] = useSearchParams();
  const agents = useResource(agentsApi.list, POLL_MS);
  const swarms = useResource(accessApi.swarms);
  const guide = useGuide(swarms.data?.length === 0 ? 'swarm' : 'agent');
  const project = params.get('project') ?? '';
  /** What was asked for here, by agent, until the server reports a change. */
  const [requests, setRequests] = useState<Record<string, { phase: string; from: string }>>({});

  // The requested state stands in for the server's while the server still
  // reports the phase the request was made in.
  const rows = useMemo(
    () =>
      agents.data?.map((agent): Row => {
        const request = requests[agentKey(agent)];
        return request && request.from === agent.phase
          ? { ...agent, phase: request.phase, requested: true }
          : agent;
      }) ?? null,
    [agents.data, requests]
  );
  const resource = { ...agents, data: rows };

  const swarmRows = swarms.data ?? [];
  const projectOptions = [
    ...new Map(swarmRows.map((swarm) => [swarm.project_id, swarm.project_name])),
  ].map(([value, label]) => ({ value, label }));
  const swarmOptions = swarmRows.map((swarm) => ({
    value: swarm.id,
    label: `${swarm.name} (${swarm.project_name})`,
    project: swarm.project_id,
  }));

  // With a project chosen and one swarm in it, that swarm is the default.
  const inProject = swarmRows.filter((swarm) => swarm.project_id === project);
  const onlySwarmOfProject = inProject.length === 1 ? inProject[0].id : '';

  const phaseLabel = (phase: string) =>
    (PHASES as readonly string[]).includes(phase)
      ? t(`brood.agents.phases.${phase}`)
      : phase || DASH;
  const roleLabel = (role: string) => role || DASH;
  const badge = (row: Row) => {
    const look = PHASE_LOOK[row.phase] ?? { tone: 'neutral' as const };
    return (
      <StatusBadge
        tone={look.tone}
        icon={look.icon}
        busy={look.busy || row.requested}
        title={row.error || undefined}
      >
        {phaseLabel(row.phase)}
      </StatusBadge>
    );
  };

  const known = (activity: string) => (ACTIVITIES as readonly string[]).includes(activity);
  const activityLabel = (activity: string) =>
    known(activity) ? t(`brood.agents.activities.${activity}`) : activity || DASH;
  /** What the verdict means, and the server's own words when it has any. */
  const activityHint = (row: Row) =>
    [
      known(row.activity) ? t(`brood.agents.activity_hints.${row.activity}`) : '',
      row.activityDetail,
    ]
      .filter(Boolean)
      .join(' ');
  /** The work of a running agent; nothing to say about any other. */
  const working = (row: Row) => row.phase === 'running' && !row.requested && row.activity !== '';
  const workBadge = (row: Row) => {
    if (!working(row)) return DASH;
    const look = ACTIVITY_LOOK[row.activity] ?? { tone: 'neutral' as const };
    return (
      <StatusBadge tone={look.tone} icon={look.icon} busy={look.busy} title={activityHint(row)}>
        {activityLabel(row.activity)}
      </StatusBadge>
    );
  };
  const count = (value: number | null) =>
    value === null ? t('brood.agents.not_measured') : String(value);

  /** Sends the action and shows its in-progress state at once; a refusal
   * takes the state back and is reported by whoever asked. */
  const request = (row: Row, verb: Verb) => async () => {
    const key = agentKey(row);
    const forget = () =>
      setRequests((prev) =>
        Object.fromEntries(Object.entries(prev).filter(([name]) => name !== key))
      );
    setRequests((prev) => ({ ...prev, [key]: { phase: REQUESTED_PHASE[verb], from: row.phase } }));
    try {
      if (verb === 'delete') await agentsApi.remove(row.swarmId, row.name);
      else await agentsApi.act(row.swarmId, row.name, verb);
    } catch (error) {
      forget();
      throw error;
    }
    window.setTimeout(forget, REQUEST_MS);
  };

  const columns: Column<Row>[] = [
    {
      id: 'name',
      header: t('brood.name'),
      primary: true,
      cell: (row) => row.name,
      text: (row) => row.name,
      sort: (row) => row.name,
    },
    {
      id: 'state',
      header: t('brood.agents.state'),
      cell: badge,
      text: (row) => `${phaseLabel(row.phase)} ${row.error}`,
      sort: (row) => phaseRank(row.phase),
    },
    {
      id: 'work',
      header: t('brood.agents.work'),
      cell: workBadge,
      text: (row) => (working(row) ? `${activityLabel(row.activity)} ${row.activityDetail}` : ''),
      sort: (row) => (working(row) ? activityRank(row.activity) : ACTIVITIES.length + 1),
    },
    {
      id: 'branch',
      header: t('brood.agents.branch'),
      cell: (row) => <Truncate mono text={working(row) ? row.branch : ''} />,
      text: (row) => row.branch,
      sort: (row) => row.branch,
    },
    {
      id: 'model',
      header: t('brood.agents.model'),
      cell: (row) => <Truncate mono text={row.model} />,
      text: (row) => row.model,
      sort: (row) => row.model,
    },
    {
      id: 'swarm',
      header: t('brood.agents.swarm'),
      cell: (row) => <Link to={agentsOf(row.swarmId, row.projectId)}>{row.swarmName}</Link>,
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
      id: 'placement',
      header: t('brood.agents.placement'),
      cell: (row) => <Truncate mono text={placement(row)} />,
      text: placement,
    },
    {
      id: 'created',
      header: t('brood.swarms.created_at'),
      cell: (row) => <RelativeTime value={row.createdAt} />,
      sort: (row) => row.createdAt,
    },
  ];

  /** The server's diagnostic in the interface language when its code is
   * known; the server's own English text otherwise. */
  const errorText = (row: Row) =>
    row.error
      ? t(`brood.agents.errors.${row.errorCode || 'other'}`, { defaultValue: row.error })
      : '';

  const terminalBlocked = (row: Row) =>
    row.phase === 'running' && !row.requested ? undefined : t('brood.agents.terminal_not_running');

  /** Everything about an agent; the server decides for real. */
  const panel = (row: Row): PanelSpec => {
    const state = phaseLabel(row.phase);
    const settled = !row.requested;
    const deleting = row.phase === 'deleting' ? t('brood.agents.is_deleting') : undefined;
    const notice = (action: string) => t('brood.agents.requested', { action, name: row.name });
    const scope = `${row.projectName}/${row.swarmName}`;
    const resources = [
      row.cpuMillicores && t('brood.agents.cpu', { count: scaled(row.cpuMillicores, 1000) }),
      row.memoryBytes && t('brood.agents.memory', { count: scaled(row.memoryBytes, GIB) }),
      row.diskBytes && t('brood.agents.disk', { count: scaled(row.diskBytes, GIB) }),
    ].filter(Boolean);
    return {
      icon: IconSidebarAgents,
      title: row.name,
      badge: badge(row),
      status: (
        <>
          <p className={styles.statusLine}>
            <span>{placement(row) || t('brood.agents.not_placed')}</span>
            <span>{roleLabel(row.role)}</span>
            <span>
              {t('brood.agents.swarm')}
              <Link to={agentsOf(row.swarmId, row.projectId)}>{row.swarmName}</Link>
            </span>
            <span>
              {t('brood.swarms.project')}
              <Link to={swarmsOf(row.projectId)}>{row.projectName}</Link>
            </span>
            <span>
              {t('brood.swarms.created_at')}
              <b>
                <RelativeTime value={row.createdAt} />
              </b>
            </span>
          </p>
          {row.error && (
            <p className={styles.errorText} role="status" title={row.error}>
              {errorText(row)}
            </p>
          )}
          {working(row) && (
            <p className={styles.statusLine}>
              <span>{workBadge(row)}</span>
              <span>{activityHint(row)}</span>
            </p>
          )}
        </>
      ),
      groups: [
        {
          id: 'work',
          label: t('brood.panel.actions'),
          actions: [
            {
              id: 'terminal',
              label: t('brood.agents.open_terminal'),
              icon: IconTerminal,
              description: t('brood.agents.terminal_hint'),
              tone: 'primary',
              href: agentsApi.terminal(row.swarmId, row.name),
              blocked: terminalBlocked(row),
            },
            {
              id: 'view',
              label: t('brood.agents.open_view'),
              icon: IconEye,
              description: t('brood.agents.view_hint'),
              href: agentsApi.view(row.swarmId, row.name),
              blocked: terminalBlocked(row),
            },
            {
              id: 'resume',
              label: t('brood.agents.start'),
              icon: IconPlay,
              description: t('brood.agents.start_hint'),
              blocked:
                settled && (row.phase === 'stopped' || row.phase === 'error')
                  ? undefined
                  : t('brood.agents.not_startable', { state }),
              run: request(row, 'resume'),
              notice: notice(t('brood.agents.start')),
            },
            {
              id: 'stop',
              label: t('brood.agents.stop'),
              icon: IconSquare,
              description: t('brood.agents.stop_hint'),
              blocked:
                settled && (row.phase === 'running' || row.phase === 'provisioning')
                  ? undefined
                  : t('brood.agents.not_stoppable', { state }),
              run: request(row, 'stop'),
              notice: notice(t('brood.agents.stop')),
            },
          ],
        },
        {
          id: 'danger',
          label: t('brood.panel.danger'),
          danger: true,
          actions: [
            {
              id: 'reset',
              label: t('brood.agents.reset'),
              icon: IconRotateCcw,
              description: t('brood.agents.reset_hint'),
              tone: 'danger',
              blocked: deleting ?? (settled ? undefined : t('brood.agents.in_progress', { state })),
              confirm: t('brood.agents.confirm_reset', { name: row.name }),
              run: request(row, 'reset'),
              notice: notice(t('brood.agents.reset')),
            },
            {
              id: 'delete',
              label: t('brood.agents.delete'),
              icon: IconTrash2,
              description: t('brood.agents.delete_hint'),
              tone: 'danger',
              blocked: deleting,
              confirm: t('brood.agents.confirm_delete', { name: row.name }),
              run: request(row, 'delete'),
              notice: notice(t('brood.agents.delete')),
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
              <dt>{t('brood.agents.swarm_id')}</dt>
              <dd>
                <Copyable value={row.swarmId} />
              </dd>
              <dt>{t('brood.agents.container')}</dt>
              <dd>{row.ctid === null ? DASH : <Copyable value={String(row.ctid)} />}</dd>
              <dt>{t('brood.agents.pool')}</dt>
              <dd>{[row.pool, row.node].filter(Boolean).join(' · ') || DASH}</dd>
              <dt>{t('brood.agents.profile')}</dt>
              <dd>{row.profile || DASH}</dd>
              <dt>{t('brood.agents.resources')}</dt>
              <dd>{resources.join(' · ') || DASH}</dd>
              <dt>{t('brood.agents.origin')}</dt>
              <dd>{row.origin ? <Copyable value={row.origin} /> : t('brood.agents.no_origin')}</dd>
              <dt>{t('brood.agents.model')}</dt>
              <dd>{row.model ? <Copyable value={row.model} /> : DASH}</dd>
              {working(row) && (
                <>
                  <dt title={t('brood.agents.work_hint')}>{t('brood.agents.work')}</dt>
                  <dd>{activityLabel(row.activity)}</dd>
                  <dt>{t('brood.agents.branch')}</dt>
                  <dd>{row.branch ? <Copyable value={row.branch} /> : DASH}</dd>
                  <dt>{t('brood.agents.uncommitted')}</dt>
                  <dd>{count(row.uncommitted)}</dd>
                  <dt>{t('brood.agents.unpushed')}</dt>
                  <dd>{count(row.unpushed)}</dd>
                </>
              )}
              <dt>{t('brood.swarms.created_at')}</dt>
              <dd>
                {row.createdAt
                  ? new Date(row.createdAt).toLocaleString(i18n.resolvedLanguage)
                  : DASH}
              </dd>
            </dl>
          </PanelSection>
          <PanelSection title={t('brood.panel.commands')}>
            <Commands
              items={[
                { label: t('brood.agents.command_join'), command: `bro join ${scope}` },
                { label: t('brood.agents.command_attach'), command: `bro attach ${row.name}` },
                {
                  label: t('brood.agents.command_send'),
                  command: `bro send ${row.name} "${t('brood.agents.command_message')}"`,
                },
                { label: t('brood.agents.command_tail'), command: `bro tail ${row.name} -f` },
              ]}
            />
          </PanelSection>
        </>
      ),
    };
  };

  return (
    <TablePage
      title={t('brood.agents.title')}
      icon={IconSidebarAgents}
      description={t('brood.agents.description')}
      create={{
        label: t('brood.agents.create'),
        sheet: (dialog) => (
          <CreateAgent
            {...dialog}
            after={agents.refresh}
            swarms={swarmRows}
            agents={agents.data ?? []}
            initialSwarm={params.get('swarm') || onlySwarmOfProject}
          />
        ),
      }}
      resource={resource}
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
        {
          param: 'state',
          label: t('brood.agents.state'),
          all: t('brood.agents.all_states'),
          options: PHASES.filter((phase) => phase !== 'resetting').map((phase) => ({
            value: phase,
            label: phaseLabel(phase),
          })),
          value: filterPhase,
          facet: true,
        },
        {
          param: 'work',
          label: t('brood.agents.work'),
          all: t('brood.agents.all_work'),
          options: ACTIVITIES.map((activity) => ({
            value: activity,
            label: activityLabel(activity),
          })),
          value: (row) => (working(row) ? row.activity : ''),
        },
      ]}
      summary={(scope) =>
        SUMMARY.map(({ phase, tone, icon }) => (
          <SummaryChip
            key={phase}
            param="state"
            value={phase}
            label={phaseLabel(phase)}
            count={scope.filter((row) => filterPhase(row) === phase).length}
            icon={icon}
            tone={tone}
          />
        ))
      }
      quick={{
        header: t('brood.agents.terminal'),
        cell: (row) => {
          const blocked = terminalBlocked(row);
          // Two ways into the session, chosen right here: typing or watching.
          const links = [
            { key: 'terminal', icon: IconTerminal, href: agentsApi.terminal(row.swarmId, row.name) },
            { key: 'view', icon: IconEye, href: agentsApi.view(row.swarmId, row.name) },
          ] as const;
          return (
            <span className={styles.quickPair}>
              {links.map(({ key, icon: LinkIcon, href }) => {
                const label = t('brood.table.action_named', {
                  action: t(`brood.agents.${key}`),
                  name: row.name,
                });
                const body = (
                  <span className={styles.withIcon}>
                    <LinkIcon size={14} />
                    {t(`brood.agents.${key}`)}
                  </span>
                );
                return blocked ? (
                  <span
                    key={key}
                    className={`btn btn-secondary btn-sm ${styles.unavailable}`}
                    role="link"
                    aria-disabled="true"
                    aria-label={label}
                    title={blocked}
                  >
                    {body}
                  </span>
                ) : (
                  <a
                    key={key}
                    className="btn btn-secondary btn-sm"
                    href={href}
                    target="_blank"
                    rel="noopener noreferrer"
                    aria-label={label}
                    title={t(`brood.agents.${key}_hint`)}
                  >
                    {body}
                  </a>
                );
              })}
            </span>
          );
        },
      }}
      panel={panel}
      defaultSort={SORT}
      empty={t('brood.agents.empty')}
      guide={guide}
    />
  );
}
