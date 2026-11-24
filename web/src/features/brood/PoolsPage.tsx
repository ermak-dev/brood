import { useTranslation } from 'react-i18next';
import {
  IconAlertTriangle,
  IconCheckCircle2,
  IconSidebarPools,
  IconTrash2,
} from '@/components/ui/icons';
import { Input } from '@/components/ui/Input';
import { poolsApi, type NewConnection, type Pool, type PoolAction } from '@/services/brood/pools';
import { useGuide } from './guide';
import { useResource } from './hooks';
import { IconBan, IconPause, IconPlay, IconPower } from './icons';
import { Field, FormSection, FormSheet } from './table/dialogs';
import { PanelSection, type PanelAction, type PanelSpec } from './table/EntityPanel';
import { useForm } from './table/formState';
import { DASH, Truncate } from './table/format';
import { TablePage, type Column } from './table/TablePage';
import { GIB, scaled } from './table/units';
import { Copyable, StatusBadge, UsageBar, type BadgeTone, type Icon } from './ui';
import broodStyles from './brood.module.scss';
import styles from './table/table.module.scss';

const SORT = { column: 'name', desc: false };

type PoolState = 'unavailable' | 'draining' | 'inactive' | 'enabled';
type Usage = NonNullable<Pool['managedPool']>;

/** In the order of the sort by state: what needs a look comes first. */
const STATES: PoolState[] = ['unavailable', 'draining', 'inactive', 'enabled'];

function poolState(pool: Pool): PoolState {
  if (!pool.managedPool) return 'inactive';
  if (pool.managedPool.state === 'draining') return 'draining';
  return pool.runtimeAvailable ? 'enabled' : 'unavailable';
}

const STATE_LOOK: Record<PoolState, { tone: BadgeTone; icon: Icon }> = {
  inactive: { tone: 'neutral', icon: IconPower },
  enabled: { tone: 'progress', icon: IconCheckCircle2 },
  draining: { tone: 'attention', icon: IconPause },
  unavailable: { tone: 'problem', icon: IconAlertTriangle },
};

/** The bars of a pool or of several: used and total in the shown units. */
const USAGE: {
  id: string;
  used: keyof Usage;
  total: keyof Usage;
  unit: number;
  unitName?: 'cores' | 'gib';
}[] = [
  { id: 'agents', used: 'usedActors', total: 'maxActors', unit: 1 },
  { id: 'cpu', used: 'usedCPUMillicores', total: 'cpuMillicores', unit: 1000, unitName: 'cores' },
  { id: 'memory', used: 'usedMemoryBytes', total: 'memoryBytes', unit: GIB, unitName: 'gib' },
  { id: 'disk', used: 'usedDiskBytes', total: 'diskBytes', unit: GIB, unitName: 'gib' },
];

const TEXT_FIELDS = [
  'name',
  'endpoint',
  'tokenID',
  'tokenSecret',
  'caPEM',
  'node',
  'resourcePool',
  'storage',
  'network',
  'template',
  'templateDigest',
] as const;
const NUMBER_FIELDS = ['actors', 'cpu', 'memoryGiB', 'diskGiB'] as const;

type TextField = (typeof TEXT_FIELDS)[number];
type DraftField = TextField | (typeof NUMBER_FIELDS)[number];
type Draft = Record<DraftField, string>;

/** The conventional values of a stock Proxmox node, so the common case is
 * the connection itself and Add. */
const DEFAULTS: Draft = {
  name: '',
  endpoint: '',
  tokenID: '',
  tokenSecret: '',
  caPEM: '',
  node: 'pve',
  resourcePool: 'brood',
  storage: 'local-lvm',
  network: 'vmbr0',
  template: 'local:vztmpl/debian-12-standard_12.7-1_amd64.tar.zst',
  templateDigest: '',
  actors: '4',
  cpu: '8',
  memoryGiB: '16',
  diskGiB: '80',
};

const EXAMPLES: Partial<Record<DraftField, string>> = {
  name: 'pve-1',
  endpoint: 'https://pve.example.com:8006',
  tokenID: 'brood@pve!panel',
  tokenSecret: 'xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx',
  node: 'pve',
  resourcePool: 'brood',
  storage: 'local-lvm',
  network: 'vmbr0',
  template: 'local:vztmpl/debian-12-standard_12.7-1_amd64.tar.zst',
  templateDigest: 'sha256:…',
};

/** What the server accepts (go/brood/server/proxmox.go,
 * proxmox_connections.go), checked here so the message sits at its field. */
const FORMATS: Partial<Record<TextField, RegExp>> = {
  name: /^[a-z0-9](?:[a-z0-9-]{0,46}[a-z0-9])?$/,
  endpoint: /^https:\/\/[^\s/?#@]+\/?$/,
  tokenID: /^[A-Za-z0-9_.-]+@[A-Za-z0-9_.-]+![A-Za-z0-9_.-]+$/,
  tokenSecret: /^[\x21-\x7e]+$/,
  caPEM: /^-----BEGIN CERTIFICATE-----[\s\S]+-----END CERTIFICATE-----$/,
  template: /^[A-Za-z0-9_-]+:vztmpl\/[A-Za-z0-9][A-Za-z0-9._-]*$/,
  templateDigest: /^(?:sha256:)?[a-f0-9]{64}$/,
};

/** What every agent of the pool gets: the server splits the budget equally
 * and rounds down to whole cores, MiB and GiB (pools.go). */
function share(draft: Draft) {
  const actors = Number(draft.actors);
  if (!NUMBER_FIELDS.every((field) => /^[1-9][0-9]{0,8}$/.test(draft[field].trim()))) return null;
  return {
    cpu: Math.floor(Number(draft.cpu) / actors),
    memoryMiB: Math.floor((Number(draft.memoryGiB) * 1024) / actors),
    diskGiB: Math.floor(Number(draft.diskGiB) / actors),
  };
}

/** The pool budget goes as the server stores it: millicores and bytes as
 * decimal strings, agents and disk GiB as numbers. A digest pasted without
 * its `sha256:` prefix gets it. */
function payload(draft: Draft): NewConnection {
  const text = (field: DraftField) => draft[field].trim();
  const digest = text('templateDigest');
  return {
    name: text('name'),
    endpoint: text('endpoint').replace(/\/$/, ''),
    tokenID: text('tokenID'),
    tokenSecret: text('tokenSecret'),
    caPEM: text('caPEM'),
    node: text('node'),
    resourcePool: text('resourcePool'),
    storage: text('storage'),
    network: text('network'),
    template: text('template'),
    templateDigest: digest.startsWith('sha256:') ? digest : `sha256:${digest}`,
    capacity: {
      actors: Number(text('actors')),
      cpuMillicores: String(Number(text('cpu')) * 1000),
      memoryBytes: String(Number(text('memoryGiB')) * GIB),
      diskGiB: Number(text('diskGiB')),
    },
  };
}

function ConnectionForm({
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
  const form = useForm(DEFAULTS, (draft) => {
    const each = share(draft);
    return {
      ...Object.fromEntries(
        TEXT_FIELDS.map((field) => {
          const value = draft[field].trim();
          const format = FORMATS[field];
          return [
            field,
            !value
              ? t('brood.form.required')
              : format && !format.test(value)
                ? t(`brood.pools.invalid.${field}`)
                : undefined,
          ];
        })
      ),
      ...(taken.includes(draft.name.trim()) ? { name: t('brood.pools.name_taken') } : {}),
      ...Object.fromEntries(
        NUMBER_FIELDS.map((field) => [
          field,
          /^[1-9][0-9]{0,8}$/.test(draft[field].trim())
            ? undefined
            : t('brood.form.positive_integer'),
        ])
      ),
      // Every agent needs a whole core and a whole GiB of disk at least.
      ...(each && (each.cpu < 1 || each.cpu > 128) ? { cpu: t('brood.pools.invalid.cpu') } : {}),
      ...(each && each.memoryMiB < 16 ? { memoryGiB: t('brood.pools.invalid.memoryGiB') } : {}),
      ...(each && each.diskGiB < 1 ? { diskGiB: t('brood.pools.invalid.diskGiB') } : {}),
    };
  });
  const input = (field: Exclude<DraftField, 'caPEM'>) => (
    <Input
      key={field}
      label={t(`brood.pools.fields.${field}`)}
      hint={t(`brood.pools.help.${field}`)}
      placeholder={EXAMPLES[field]}
      required
      type={field === 'tokenSecret' ? 'password' : 'text'}
      inputMode={(NUMBER_FIELDS as readonly string[]).includes(field) ? 'numeric' : undefined}
      autoComplete={field === 'tokenSecret' ? 'new-password' : 'off'}
      autoCapitalize="off"
      spellCheck={false}
      {...form.field(field)}
    />
  );
  const { error: caError, ...ca } = form.field('caPEM');
  const name = form.values.name.trim();
  const each = share(form.values);
  return (
    <FormSheet
      open={open}
      close={close}
      title={t('brood.pools.add')}
      icon={IconSidebarPools}
      description={t('brood.pools.form_hint')}
      submitLabel={t('brood.pools.add')}
      check={form.check}
      dirty={form.dirty}
      submit={() => poolsApi.createConnection(payload(form.values))}
      success={t('brood.pools.added', { name })}
      after={after}
      size="lg"
    >
      <FormSection
        title={t('brood.pools.sections.connection')}
        hint={t('brood.pools.sections.connection_hint')}
      >
        <div className={styles.grid2}>
          {input('name')}
          {input('endpoint')}
          {input('tokenID')}
          {input('tokenSecret')}
        </div>
        <Field
          label={t('brood.pools.fields.caPEM')}
          required
          hint={t('brood.pools.help.caPEM')}
          error={caError}
        >
          {(id) => (
            <textarea
              id={id}
              className={`input ${styles.textarea}`}
              placeholder={'-----BEGIN CERTIFICATE-----\n…\n-----END CERTIFICATE-----'}
              spellCheck={false}
              autoComplete="off"
              aria-invalid={Boolean(caError)}
              {...ca}
            />
          )}
        </Field>
      </FormSection>
      <FormSection
        title={t('brood.pools.sections.placement')}
        hint={t('brood.pools.sections.placement_hint')}
      >
        <div className={styles.grid2}>
          {input('node')}
          {input('resourcePool')}
          {input('storage')}
          {input('network')}
        </div>
      </FormSection>
      <FormSection
        title={t('brood.pools.sections.template')}
        hint={t('brood.pools.sections.template_hint')}
      >
        {input('template')}
        {input('templateDigest')}
      </FormSection>
      <FormSection
        title={t('brood.pools.sections.capacity')}
        hint={t('brood.pools.sections.capacity_hint')}
      >
        <div className={styles.grid2}>{NUMBER_FIELDS.map((field) => input(field))}</div>
        {each && (
          <p className={styles.fieldHint} aria-live="polite">
            {t('brood.pools.share', {
              cpu: each.cpu,
              memory: Math.round((each.memoryMiB / 1024) * 10) / 10,
              disk: each.diskGiB,
            })}
          </p>
        )}
      </FormSection>
    </FormSheet>
  );
}

export function PoolsPage() {
  const { t } = useTranslation();
  const pools = useResource(poolsApi.list);
  const guide = useGuide('pool');
  const stateLabel = (pool: Pool) => t(`brood.pools.states.${poolState(pool)}`);
  const badge = (pool: Pool) => {
    const look = STATE_LOOK[poolState(pool)];
    return (
      <StatusBadge
        tone={look.tone}
        icon={look.icon}
        title={pool.runtimeAvailable ? undefined : t('brood.pools.no_runtime')}
      >
        {stateLabel(pool)}
      </StatusBadge>
    );
  };
  const bars = (usage: Usage[]) =>
    USAGE.map(({ id, used, total, unit, unitName }) => (
      <UsageBar
        key={id}
        label={t(`brood.pools.usage.${id}`)}
        used={usage.reduce((sum, pool) => sum + scaled(pool[used], unit), 0)}
        total={usage.reduce((sum, pool) => sum + scaled(pool[total], unit), 0)}
        unit={unitName && t(`brood.pools.units.${unitName}`)}
      />
    ));

  const used = (
    id: string,
    read: (pool: Usage) => string,
    key: (pool: Usage) => number
  ): Column<Pool> => ({
    id,
    header: t(`brood.pools.${id}`),
    numeric: true,
    cell: (row) => (row.managedPool ? read(row.managedPool) : DASH),
    sort: (row) => (row.managedPool ? key(row.managedPool) : -1),
  });
  const ratio = (pool: Usage, usedField: keyof Usage, total: keyof Usage, unit = 1) =>
    `${scaled(pool[usedField], unit)} / ${scaled(pool[total], unit)}`;
  const columns: Column<Pool>[] = [
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
      header: t('brood.pools.state'),
      cell: badge,
      text: stateLabel,
      sort: (row) => STATES.indexOf(poolState(row)),
    },
    {
      id: 'connection',
      header: t('brood.pools.connection'),
      cell: (row) => <Truncate mono text={`${row.endpoint} · ${row.node}`} />,
      text: (row) => `${row.endpoint} ${row.node}`,
    },
    used(
      'agents',
      (pool) => ratio(pool, 'usedActors', 'maxActors'),
      (pool) => Number(pool.usedActors)
    ),
    used(
      'cpu',
      (pool) => ratio(pool, 'usedCPUMillicores', 'cpuMillicores', 1000),
      (pool) => Number(pool.usedCPUMillicores)
    ),
    used(
      'memory',
      (pool) => ratio(pool, 'usedMemoryBytes', 'memoryBytes', GIB),
      (pool) => Number(pool.usedMemoryBytes)
    ),
    used(
      'disk',
      (pool) => ratio(pool, 'usedDiskBytes', 'diskBytes', GIB),
      (pool) => Number(pool.usedDiskBytes)
    ),
  ];

  const panel = (row: Pool): PanelSpec => {
    const state = poolState(row);
    const usage = row.managedPool;
    const placed = Number(usage?.usedActors ?? 0);
    const act = (
      action: PoolAction,
      icon: Icon,
      blocked: string | undefined,
      tone?: 'primary'
    ): PanelAction => ({
      id: action,
      label: t(`brood.pools.${action}`),
      icon,
      description: t(`brood.pools.${action}_hint`),
      blocked,
      tone,
      run: () => poolsApi.act(row.name, action),
    });
    const { uid } = row;
    return {
      icon: IconSidebarPools,
      title: row.name,
      badge: badge(row),
      status: (
        <>
          <p className={styles.statusLine}>
            <span className={styles.mono}>{row.endpoint}</span>
            <span>
              {t('brood.pools.fields.node')}
              <b>{row.node}</b>
            </span>
          </p>
          {!row.runtimeAvailable && <p className={styles.errorText}>{t('brood.pools.no_runtime')}</p>}
          {usage && bars([usage])}
        </>
      ),
      groups: [
        {
          id: 'work',
          label: t('brood.panel.actions'),
          actions: [
            act(
              'activate',
              IconPower,
              usage
                ? t('brood.pools.already_active')
                : row.runtimeAvailable
                  ? undefined
                  : t('brood.pools.no_runtime'),
              usage ? undefined : 'primary'
            ),
            act(
              'drain',
              IconPause,
              state === 'enabled' || state === 'unavailable'
                ? undefined
                : t(usage ? 'brood.pools.already_draining' : 'brood.pools.not_active')
            ),
            act(
              'enable',
              IconPlay,
              state === 'draining'
                ? undefined
                : t(usage ? 'brood.pools.not_draining' : 'brood.pools.not_active')
            ),
          ],
        },
        {
          id: 'danger',
          label: t('brood.panel.danger'),
          danger: true,
          actions: [
            {
              id: 'deactivate',
              label: t('brood.pools.deactivate'),
              icon: IconBan,
              description: t('brood.pools.deactivate_hint'),
              tone: 'danger',
              blocked: !usage
                ? t('brood.pools.not_active')
                : placed > 0
                  ? t('brood.pools.has_agents', { count: placed })
                  : undefined,
              confirm: t('brood.pools.confirm_deactivate', { name: row.name }),
              run: () => poolsApi.deletePool(row.name),
            },
            {
              id: 'delete',
              label: t('brood.pools.delete_connection'),
              icon: IconTrash2,
              description: t('brood.pools.delete_connection_hint'),
              tone: 'danger',
              // A connection from the server's .env has no record to delete.
              blocked:
                uid === undefined
                  ? t('brood.pools.from_env')
                  : usage
                    ? t('brood.pools.deactivate_first')
                    : undefined,
              confirm: t('brood.pools.confirm_delete_connection', { name: row.name }),
              run: () => poolsApi.deleteConnection(uid ?? ''),
            },
          ],
        },
      ],
      details: (
        <PanelSection title={t('brood.panel.details')}>
          <dl className={broodStyles.details}>
            <dt>{t('brood.pools.fields.endpoint')}</dt>
            <dd>
              <Copyable value={row.endpoint} />
            </dd>
            <dt>{t('brood.pools.fields.node')}</dt>
            <dd>{row.node || DASH}</dd>
            <dt>{t('brood.pools.fields.tokenID')}</dt>
            <dd>{row.tokenID || DASH}</dd>
            <dt>{t('brood.pools.fields.resourcePool')}</dt>
            <dd>{row.resourcePool || DASH}</dd>
            <dt>{t('brood.pools.fields.storage')}</dt>
            <dd>{row.storage || DASH}</dd>
            <dt>{t('brood.pools.fields.network')}</dt>
            <dd>{row.network || DASH}</dd>
            <dt>{t('brood.pools.source')}</dt>
            <dd>{t(uid === undefined ? 'brood.pools.source_env' : 'brood.pools.source_panel')}</dd>
          </dl>
        </PanelSection>
      ),
    };
  };

  return (
    <TablePage
      title={t('brood.pools.title')}
      icon={IconSidebarPools}
      description={t('brood.pools.description')}
      create={{
        label: t('brood.pools.add'),
        sheet: (dialog) => (
          <ConnectionForm
            {...dialog}
            after={pools.refresh}
            taken={(pools.data ?? []).map((pool) => pool.name)}
          />
        ),
      }}
      resource={pools}
      rowKey={(row) => row.name}
      rowName={(row) => row.name}
      columns={columns}
      summary={(scope) => {
        const active = scope.flatMap((pool) => (pool.managedPool ? [pool.managedPool] : []));
        return (
          active.length > 0 && (
            <div className={styles.usageCard} aria-label={t('brood.pools.usage.title')}>
              {bars(active)}
            </div>
          )
        );
      }}
      panel={panel}
      defaultSort={SORT}
      empty={t('brood.pools.empty')}
      guide={guide}
    />
  );
}
