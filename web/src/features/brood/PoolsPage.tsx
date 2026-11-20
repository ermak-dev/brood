import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { EmptyState } from '@/components/ui/EmptyState';
import { Input } from '@/components/ui/Input';
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/Table';
import { poolsApi, type NewConnection, type Pool, type PoolAction } from '@/services/brood/pools';
import { useActions, useResource } from './hooks';
import { DASH, Field, FormModal, LoadState, PageHeader, StatusBadge, type BadgeTone } from './ui';
import styles from './brood.module.scss';

const GIB = 1073741824;

/** Used of total, in the unit of the column; decimal strings from the server. */
const usage = (used: string, total: string, unit = 1) =>
  `${Math.round((Number(used) / unit) * 10) / 10} / ${Math.round((Number(total) / unit) * 10) / 10}`;

type PoolState = 'inactive' | 'unavailable' | 'enabled' | 'draining';

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
  'node',
  'resourcePool',
  'storage',
  'network',
  'template',
  'templateDigest',
] as const;
const NUMBER_FIELDS = ['actors', 'cpu', 'memoryGiB', 'diskGiB'] as const;

type DraftField = (typeof TEXT_FIELDS)[number] | (typeof NUMBER_FIELDS)[number] | 'caPEM';
type Draft = Record<DraftField, string>;

const EMPTY: Draft = {
  name: '',
  endpoint: '',
  tokenID: '',
  tokenSecret: '',
  caPEM: '',
  node: '',
  resourcePool: '',
  storage: '',
  network: '',
  template: '',
  templateDigest: '',
  actors: '',
  cpu: '',
  memoryGiB: '',
  diskGiB: '',
};

const complete = (draft: Draft) =>
  [...TEXT_FIELDS, 'caPEM' as const].every((field) => draft[field].trim() !== '') &&
  NUMBER_FIELDS.every((field) => /^[1-9][0-9]{0,8}$/.test(draft[field].trim()));

/** The pool budget goes as the server stores it: millicores and bytes as
 * decimal strings, workers and disk GiB as numbers. */
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
  run,
  close,
}: {
  open: boolean;
  run: (action: () => Promise<unknown>) => Promise<boolean>;
  close: () => void;
}) {
  const { t } = useTranslation();
  const [draft, setDraft] = useState(EMPTY);
  const input = (field: DraftField, numeric = false) => (
    <Input
      key={field}
      label={t(`brood.pools.fields.${field}`)}
      value={draft[field]}
      type={field === 'tokenSecret' ? 'password' : 'text'}
      inputMode={numeric ? 'numeric' : undefined}
      autoComplete={field === 'tokenSecret' ? 'new-password' : 'off'}
      autoCapitalize="off"
      spellCheck={false}
      autoFocus={field === 'name'}
      onChange={(event) => setDraft({ ...draft, [field]: event.target.value })}
    />
  );
  return (
    <FormModal
      open={open}
      title={t('brood.pools.add')}
      submitLabel={t('brood.create')}
      canSubmit={complete(draft)}
      submit={() => run(() => poolsApi.createConnection(payload(draft)))}
      close={close}
      width={680}
    >
      <p className={styles.fieldHint}>{t('brood.pools.form_hint')}</p>
      <div className={styles.grid2}>
        {input('name')}
        {input('endpoint')}
        {input('tokenID')}
        {input('tokenSecret')}
      </div>
      <Field label={t('brood.pools.fields.caPEM')}>
        {(id) => (
          <textarea
            id={id}
            className={`input ${styles.textarea}`}
            value={draft.caPEM}
            spellCheck={false}
            autoComplete="off"
            onChange={(event) => setDraft({ ...draft, caPEM: event.target.value })}
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
    </FormModal>
  );
}

export function PoolsPage() {
  const { t } = useTranslation();
  const pools = useResource(poolsApi.list);
  const { run, confirm, busy } = useActions(pools.refresh);
  // Each opening is a new form (session): the secret does not outlive its dialog.
  const [dialog, setDialog] = useState({ open: false, session: 0 });
  const rows = pools.data ?? [];
  const act = (pool: Pool, action: PoolAction) => (
    <Button
      variant="secondary"
      size="sm"
      disabled={busy}
      aria-label={t(`brood.pools.${action}_named`, { name: pool.name })}
      onClick={() => void run(() => poolsApi.act(pool.name, action))}
    >
      {t(`brood.pools.${action}`)}
    </Button>
  );
  const remove = (pool: Pool, kind: 'pool' | 'connection', action: () => Promise<unknown>) => (
    <Button
      variant="ghost"
      size="sm"
      className={styles.dangerText}
      disabled={busy}
      aria-label={t(`brood.pools.delete_${kind}_named`, { name: pool.name })}
      onClick={() =>
        confirm({
          title: t(`brood.pools.delete_${kind}`),
          message: t(`brood.pools.confirm_delete_${kind}`, { name: pool.name }),
          action,
        })
      }
    >
      {t(`brood.pools.delete_${kind}`)}
    </Button>
  );
  return (
    <div className={styles.page}>
      <PageHeader
        title={t('brood.pools.title')}
        actions={
          <Button onClick={() => setDialog({ open: true, session: dialog.session + 1 })}>
            {t('brood.pools.add')}
          </Button>
        }
      />
      <LoadState {...pools} ready={pools.data !== null} retry={pools.refresh}>
        {rows.length === 0 ? (
          <EmptyState title={t('brood.pools.empty')} />
        ) : (
          <Table>
            <TableHeader>
              <tr>
                <TableHead>{t('brood.name')}</TableHead>
                <TableHead>{t('brood.pools.connection')}</TableHead>
                <TableHead>{t('brood.pools.state')}</TableHead>
                <TableHead alignRight>{t('brood.pools.workers')}</TableHead>
                <TableHead alignRight>{t('brood.pools.cpu')}</TableHead>
                <TableHead alignRight>{t('brood.pools.memory')}</TableHead>
                <TableHead alignRight>{t('brood.pools.disk')}</TableHead>
                <TableHead />
              </tr>
            </TableHeader>
            <TableBody>
              {rows.map((pool) => {
                const state = poolState(pool);
                const managed = pool.managedPool;
                return (
                  <TableRow key={pool.name}>
                    <TableCell>
                      <span className={styles.nameLink}>{pool.name}</span>
                    </TableCell>
                    <TableCell className={styles.mono}>
                      {pool.endpoint} · {pool.node}
                    </TableCell>
                    <TableCell>
                      <StatusBadge tone={stateTone[state]}>
                        {t(`brood.pools.states.${state}`)}
                      </StatusBadge>
                      {!managed && !pool.runtimeAvailable && (
                        <p className={styles.errorText}>{t('brood.pools.no_runtime')}</p>
                      )}
                    </TableCell>
                    <TableCell alignRight>
                      {managed ? usage(managed.usedActors, managed.maxActors) : DASH}
                    </TableCell>
                    <TableCell alignRight>
                      {managed
                        ? usage(managed.usedCPUMillicores, managed.cpuMillicores, 1000)
                        : DASH}
                    </TableCell>
                    <TableCell alignRight>
                      {managed ? usage(managed.usedMemoryBytes, managed.memoryBytes, GIB) : DASH}
                    </TableCell>
                    <TableCell alignRight>
                      {managed ? usage(managed.usedDiskBytes, managed.diskBytes, GIB) : DASH}
                    </TableCell>
                    <TableCell>
                      <div className={styles.rowActions}>
                        {!managed && pool.runtimeAvailable && act(pool, 'activate')}
                        {managed && state === 'draining' && act(pool, 'enable')}
                        {managed && state !== 'draining' && act(pool, 'drain')}
                        {managed
                          ? remove(pool, 'pool', () => poolsApi.deletePool(pool.name))
                          : pool.uid !== undefined &&
                            remove(pool, 'connection', () =>
                              poolsApi.deleteConnection(pool.uid ?? '')
                            )}
                      </div>
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        )}
      </LoadState>
      {dialog.session > 0 && (
        <ConnectionForm
          key={dialog.session}
          open={dialog.open}
          run={run}
          close={() => setDialog({ ...dialog, open: false })}
        />
      )}
    </div>
  );
}
