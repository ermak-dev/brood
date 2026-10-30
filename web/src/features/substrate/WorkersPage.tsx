import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom';
import { Button } from '@/components/ui/Button';
import { EmptyState } from '@/components/ui/EmptyState';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';
import { workersApi, type Worker, type WorkerResources } from '@/services/brood/substrate';
import { EditorSheet } from './components/EditorSheet';
import { EntityTable, type Column } from './components/EntityTable';
import { Section } from './components/form';
import { PageHeader } from './components/PageHeader';
import { StateBadge } from './components/StateBadge';
import { DASH, formatCpu, formatDate, formatMemory, shortState } from './format';
import { useActions, useResource } from './hooks';
import { workerAction } from './workerActions';
import styles from './substrate.module.scss';

const stateOf = (worker: Worker) => shortState(worker.status?.state, 'WORKER_STATE_');

/** 'allocated / capacity' of one resource, formatted. */
function usage(
  worker: Worker,
  pick: (r: WorkerResources | undefined) => string | number | undefined,
  format: (v: string | number | undefined) => string = (v) => String(v ?? 0)
): string {
  const capacity = pick(worker.status?.capacity);
  if (!capacity) return DASH;
  return `${format(pick(worker.status?.allocated) ?? 0)} / ${format(capacity)}`;
}

/** brood: the install's workers -- mirrors of the pool's pods that
 * ate-controller writes (labels from the WorkerPool, availability from pod
 * readiness), so a hand edit would be undone at the next sync.
 *
 * Draining is final: the record never becomes schedulable again, while its pod
 * keeps running and holding the pool's place. Replacing is the way back --
 * the pod is deleted and the Deployment brings up a fresh, schedulable one. */
export function WorkersPage() {
  const { t, i18n } = useTranslation();
  const navigate = useNavigate();
  const { data, error, loading, refresh } = useResource(workersApi.list);
  const { confirm } = useActions(refresh);
  const [viewing, setViewing] = useState<Worker | null>(null);

  const drain = (worker: Worker) =>
    confirm({
      title: t('workers.drain_title'),
      message: t('workers.drain_message', { name: worker.metadata.name }),
      confirmText: t('workers.drain'),
      success: t('workers.drained', { name: worker.metadata.name }),
      run: async () => {
        await workersApi.drain(worker.metadata.name);
        setViewing(null);
      },
    });

  const replace = (worker: Worker) =>
    confirm({
      title: t('workers.replace_title'),
      message: t('workers.replace_message', { name: worker.metadata.name }),
      confirmText: t('workers.replace'),
      success: t('workers.replaced', { name: worker.metadata.name }),
      run: async () => {
        await workersApi.replace(worker.metadata.name);
        setViewing(null);
      },
    });

  // Одна кнопка строки и карточки: что предложить, решает состояние.
  const action = (worker: Worker, inRow: boolean) => {
    const next = workerAction(stateOf(worker));
    if (!next) return null;
    const recovery = next === 'replace';
    return (
      <Button
        variant={recovery ? (inRow ? 'secondary' : 'primary') : inRow ? 'ghost' : 'danger'}
        size={inRow ? 'sm' : undefined}
        className={!recovery && inRow ? styles.dangerText : undefined}
        onClick={() => (recovery ? replace(worker) : drain(worker))}
      >
        {t(recovery ? 'workers.replace' : 'workers.drain')}
      </Button>
    );
  };

  const columns: Column<Worker>[] = [
    { key: 'name', label: t('substrate.name'), primary: true, render: (w) => w.metadata.name },
    {
      key: 'state',
      label: t('substrate.state'),
      render: (w) => <StateBadge state={stateOf(w)} namespace="workers.state" />,
    },
    { key: 'pool', label: t('workers.pool'), mono: true, render: (w) => w.workerPool || DASH },
    { key: 'node', label: t('workers.node'), hideOnMobile: true, render: (w) => w.nodeName || DASH },
    { key: 'actors', label: t('workers.actors'), render: (w) => usage(w, (r) => r?.actors) },
    {
      key: 'cpu',
      label: 'CPU',
      hideOnMobile: true,
      render: (w) => usage(w, (r) => r?.cpuMillicores, formatCpu),
    },
    {
      key: 'memory',
      label: t('workers.memory'),
      hideOnMobile: true,
      render: (w) => usage(w, (r) => r?.memoryBytes, formatMemory),
    },
  ];

  const count = (state: string) => (data ?? []).filter((w) => stateOf(w) === state).length;

  return (
    <div className={styles.page}>
      <PageHeader
        title={t('workers.title')}
        meta={[
          data && t('workers.meta_count', { count: data.length }),
          data && count('active') > 0 && t('workers.meta_active', { count: count('active') }),
          data && count('draining') > 0 && t('workers.meta_draining', { count: count('draining') }),
        ]}
      />
      <p className={styles.notice}>{t('workers.managed_hint')}</p>
      {count('draining') > 0 && (
        <p className={styles.notice} role="note">{t('workers.drained_hint', { count: count('draining') })}</p>
      )}

      {loading ? (
        <LoadingSpinner />
      ) : error ? (
        <div className={styles.empty}>
          <EmptyState title={t('substrate.load_failed')} description={error} />
        </div>
      ) : data && data.length > 0 ? (
        <EntityTable
          columns={columns}
          rows={data}
          rowKey={(w) => w.metadata.uid ?? w.metadata.name}
          onRowClick={setViewing}
          actionsLabel={t('substrate.actions')}
          actions={(w) => action(w, true)}
        />
      ) : (
        <div className={styles.empty}>
          <EmptyState title={t('workers.empty')} description={t('workers.empty_hint')}
            action={<Button onClick={() => navigate('/worker-pools')}>{t('nav.worker_pools')}</Button>} />
        </div>
      )}

      <EditorSheet
        open={viewing !== null}
        onClose={() => setViewing(null)}
        size="lg"
        eyebrow={viewing?.workerPool}
        title={viewing?.metadata.name ?? ''}
        extra={viewing && action(viewing, false)}
      >
        {viewing && (
          <div className={styles.form}>
            <Section title={t('workers.placement')}>
              <dl className={styles.details}>
                <dt>{t('substrate.state')}</dt>
                <dd>
                  <StateBadge state={stateOf(viewing)} namespace="workers.state" />
                </dd>
                <dt>{t('workers.node')}</dt>
                <dd>{viewing.nodeName || DASH}</dd>
                <dt>{t('workers.pod')}</dt>
                <dd className={styles.mono}>
                  {viewing.workerNamespace ? `${viewing.workerNamespace}/` : ''}
                  {viewing.workerPod || DASH}
                </dd>
                <dt>IP</dt>
                <dd className={styles.mono}>{viewing.status?.ips?.join(', ') || DASH}</dd>
                <dt>SPIFFE ID</dt>
                <dd className={styles.mono}>{viewing.workerSpiffeId || DASH}</dd>
                <dt>{t('substrate.created')}</dt>
                <dd>{formatDate(viewing.metadata.createTime, i18n.language)}</dd>
              </dl>
            </Section>
            <Section title={t('workers.load')}>
              <dl className={styles.details}>
                <dt>{t('workers.actors')}</dt>
                <dd>{usage(viewing, (r) => r?.actors)}</dd>
                <dt>CPU</dt>
                <dd>{usage(viewing, (r) => r?.cpuMillicores, formatCpu)}</dd>
                <dt>{t('workers.memory')}</dt>
                <dd>{usage(viewing, (r) => r?.memoryBytes, formatMemory)}</dd>
              </dl>
            </Section>
            <Section title={t('workers.labels')} hint={t('workers.labels_hint')}>
              {Object.keys(viewing.labels ?? {}).length ? (
                <div className={styles.chips}>
                  {Object.entries(viewing.labels ?? {}).map(([key, value]) => (
                    <span key={key} className={`${styles.chip} ${styles.chipStatic}`}>
                      {key}={value}
                    </span>
                  ))}
                </div>
              ) : (
                <span className={styles.muted}>{DASH}</span>
              )}
            </Section>
          </div>
        )}
      </EditorSheet>
    </div>
  );
}
