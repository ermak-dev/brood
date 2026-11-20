import { useTranslation } from 'react-i18next';
import { Input } from '@/components/ui/Input';
import { poolsApi, type NewConnection, type Pool, type PoolAction } from '@/services/brood/pools';
import { useResource } from './hooks';
import { Field, FormDialog } from './table/dialogs';
import { useDialog, useForm } from './table/formState';
import { DASH, Truncate } from './table/format';
import { TablePage, type Column, type RowAction } from './table/TablePage';
import { StatusBadge, type BadgeTone } from './ui';
import styles from './table/table.module.scss';

const GIB = 1073741824;
const SORT = { column: 'name', desc: false };

/** Used of total, in the unit of the column; decimal strings from the server. */
const usage = (used: string, total: string, unit = 1) =>
  `${Math.round((Number(used) / unit) * 10) / 10} / ${Math.round((Number(total) / unit) * 10) / 10}`;

type PoolState = 'unavailable' | 'draining' | 'inactive' | 'enabled';

/** In the order of the sort by state: what needs a look comes first. */
const STATES: PoolState[] = ['unavailable', 'draining', 'inactive', 'enabled'];

function poolState(pool: Pool): PoolState {
  if (!pool.managedPool) return 'inactive';
  if (pool.managedPool.state === 'draining') return 'draining';
  return pool.runtimeAvailable ? 'enabled' : 'unavailable';
}

const stateTone: Record<PoolState, BadgeTone> = {
  inactive: 'neutral',
  enabled: 'neutral',
  draining: 'progress',
  unavailable: 'problem',
};

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

type DraftField = (typeof TEXT_FIELDS)[number] | (typeof NUMBER_FIELDS)[number];
type Draft = Record<DraftField, string>;

const EMPTY = Object.fromEntries(
  [...TEXT_FIELDS, ...NUMBER_FIELDS].map((field) => [field, ''])
) as Draft;

/** The pool budget goes as the server stores it: millicores and bytes as
 * decimal strings, agents and disk GiB as numbers. */
function payload(draft: Draft): NewConnection {
  const text = (field: DraftField) => draft[field].trim();
  return {
    name: text('name'),
    endpoint: text('endpoint'),
    tokenID: text('tokenID'),
    tokenSecret: text('tokenSecret'),
    caPEM: text('caPEM'),
    node: text('node'),
    resourcePool: text('resourcePool'),
    storage: text('storage'),
    network: text('network'),
    template: text('template'),
    templateDigest: text('templateDigest'),
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
}: {
  open: boolean;
  close: () => void;
  after: () => unknown;
}) {
  const { t } = useTranslation();
  const form = useForm(EMPTY, (draft) => ({
    ...Object.fromEntries(
      TEXT_FIELDS.map((field) => [
        field,
        draft[field].trim() ? undefined : t('brood.form.required'),
      ])
    ),
    ...Object.fromEntries(
      NUMBER_FIELDS.map((field) => [
        field,
        /^[1-9][0-9]{0,8}$/.test(draft[field].trim())
          ? undefined
          : t('brood.form.positive_integer'),
      ])
    ),
    ...(draft.endpoint.trim() && !/^https:\/\/\S+$/.test(draft.endpoint.trim())
      ? { endpoint: t('brood.pools.endpoint_invalid') }
      : {}),
  }));
  const input = (field: Exclude<DraftField, 'caPEM'>, numeric = false) => (
    <Input
      key={field}
      label={t(`brood.pools.fields.${field}`)}
      required
      type={field === 'tokenSecret' ? 'password' : 'text'}
      inputMode={numeric ? 'numeric' : undefined}
      autoComplete={field === 'tokenSecret' ? 'new-password' : 'off'}
      autoCapitalize="off"
      spellCheck={false}
      {...form.field(field)}
    />
  );
  const { error: caError, ...ca } = form.field('caPEM');
  const name = form.values.name.trim();
  return (
    <FormDialog
      open={open}
      close={close}
      title={t('brood.pools.add')}
      submitLabel={t('brood.pools.add')}
      valid={form.valid}
      submit={() => poolsApi.createConnection(payload(form.values))}
      success={t('brood.pools.added', { name })}
      after={after}
      width={680}
    >
      <p className={styles.fieldHint}>{t('brood.pools.form_hint')}</p>
      <div className={styles.grid2}>
        {input('name')}
        {input('endpoint')}
        {input('tokenID')}
        {input('tokenSecret')}
      </div>
      <Field label={t('brood.pools.fields.caPEM')} required error={caError}>
        {(id) => (
          <textarea
            id={id}
            className={`input ${styles.textarea}`}
            spellCheck={false}
            autoComplete="off"
            aria-invalid={Boolean(caError)}
            {...ca}
          />
        )}
      </Field>
      <div className={styles.grid2}>
        {input('node')}
        {input('resourcePool')}
        {input('storage')}
        {input('network')}
        {input('template')}
        {input('templateDigest')}
        {NUMBER_FIELDS.map((field) => input(field, true))}
      </div>
    </FormDialog>
  );
}

export function PoolsPage() {
  const { t } = useTranslation();
  const pools = useResource(poolsApi.list);
  const create = useDialog();
  const stateLabel = (pool: Pool) => t(`brood.pools.states.${poolState(pool)}`);

  const used = (
    id: string,
    read: (pool: NonNullable<Pool['managedPool']>) => string
  ): Column<Pool> => ({
    id,
    header: t(`brood.pools.${id}`),
    numeric: true,
    cell: (row) => (row.managedPool ? read(row.managedPool) : DASH),
  });
  const columns: Column<Pool>[] = [
    {
      id: 'name',
      header: t('brood.name'),
      cell: (row) => <span className={styles.name}>{row.name}</span>,
      text: (row) => row.name,
      sort: (row) => row.name,
    },
    {
      id: 'connection',
      header: t('brood.pools.connection'),
      cell: (row) => <Truncate mono text={`${row.endpoint} · ${row.node}`} />,
      text: (row) => `${row.endpoint} ${row.node}`,
    },
    {
      id: 'state',
      header: t('brood.pools.state'),
      cell: (row) => (
        <span
          title={
            !row.managedPool && !row.runtimeAvailable ? t('brood.pools.no_runtime') : undefined
          }
        >
          <StatusBadge tone={stateTone[poolState(row)]}>{stateLabel(row)}</StatusBadge>
        </span>
      ),
      text: stateLabel,
      sort: (row) => STATES.indexOf(poolState(row)),
    },
    used('agents', (pool) => usage(pool.usedActors, pool.maxActors)),
    used('cpu', (pool) => usage(pool.usedCPUMillicores, pool.cpuMillicores, 1000)),
    used('memory', (pool) => usage(pool.usedMemoryBytes, pool.memoryBytes, GIB)),
    used('disk', (pool) => usage(pool.usedDiskBytes, pool.diskBytes, GIB)),
  ];

  const actions = (row: Pool): RowAction[] => {
    const act = (action: PoolAction, disabled?: string): RowAction => ({
      id: action,
      label: t(`brood.pools.${action}`),
      inline: true,
      disabled,
      run: () => poolsApi.act(row.name, action),
    });
    const { uid } = row;
    return [
      !row.managedPool
        ? act('activate', row.runtimeAvailable ? undefined : t('brood.pools.no_runtime'))
        : poolState(row) === 'draining'
          ? act('enable')
          : act('drain'),
      row.managedPool
        ? {
            id: 'deactivate',
            label: t('brood.pools.deactivate'),
            danger: true,
            confirm: t('brood.pools.confirm_deactivate', { name: row.name }),
            run: () => poolsApi.deletePool(row.name),
          }
        : {
            id: 'delete',
            label: t('brood.pools.delete_connection'),
            danger: true,
            // A connection from the server's .env has no record to delete.
            disabled: uid === undefined ? t('brood.pools.from_env') : undefined,
            confirm: t('brood.pools.confirm_delete_connection', { name: row.name }),
            run: () => poolsApi.deleteConnection(uid ?? ''),
          },
    ];
  };

  return (
    <TablePage
      title={t('brood.pools.title')}
      description={t('brood.pools.description')}
      primary={{ label: t('brood.pools.add'), onClick: () => create.show(null) }}
      resource={pools}
      rowKey={(row) => row.name}
      rowName={(row) => row.name}
      columns={columns}
      defaultSort={SORT}
      empty={t('brood.pools.empty')}
      actions={actions}
    >
      {create.mount((dialog) => (
        <ConnectionForm {...dialog} after={pools.refresh} />
      ))}
    </TablePage>
  );
}
