import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom';
import { Button } from '@/components/ui/Button';
import { EmptyState } from '@/components/ui/EmptyState';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';
import { workersApi, type Worker, type WorkerResources } from '@/services/brood/substrate';
import { RelatedLink } from '@/features/objects/ObjectLink';
import { workerPool } from '@/features/objects/relations';
import { useObjectSelection, usePanel } from '@/features/objects/useObjectSelection';
import { EntityPanel, type PanelActionGroup } from './components/EntityPanel';
import { EntityTable, type Column } from './components/EntityTable';
import { Section } from './components/form';
import { PageHeader } from './components/PageHeader';
import { ObjectUnavailable } from './components/SelectionNotice';
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
  // Панель воркера -- по адресу (?worker=, #194), воркер -- из свежего списка.
  const { selection, open, close } = useObjectSelection();
  const panel = usePanel(selection.worker);
  const viewing = (panel.name && data?.find((w) => w.metadata.name === panel.name)) || null;
  const closePanel = () => close('worker');
  const unavailable = Boolean(
    selection.worker && data && !data.some((w) => w.metadata.name === selection.worker)
  );

  // Единственное действие воркера зависит от состояния: вывод из работы
  // необратим (отдельный блок), восстановление ёмкости — обычное.
  const actionGroups = (worker: Worker): PanelActionGroup[] => {
    const next = workerAction(stateOf(worker));
    if (!next) return [];
    const run = () => confirm({
      title: t(`workers.${next}_title`),
      message: t(`workers.${next}_message`, { name: worker.metadata.name }),
      confirmText: t(`workers.${next}`),
      success: t(next === 'replace' ? 'workers.replaced' : 'workers.drained', {
        name: worker.metadata.name,
      }),
      run: async () => {
        await workersApi[next](worker.metadata.name);
        closePanel();
      },
    });
    const action = {
      id: next,
      label: t(`workers.${next}`),
      description: t(`workers.${next}_hint`),
      tone: next === 'drain' ? ('danger' as const) : ('primary' as const),
      onSelect: run,
    };
    return next === 'drain'
      ? [{ id: 'danger', label: t('substrate.panel_danger'), danger: true, actions: [action] }]
      : [{ id: 'main', label: t('substrate.actions'), actions: [action] }];
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
      {unavailable && selection.worker && (
        <ObjectUnavailable name={selection.worker} onClose={closePanel} />
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
          onRowClick={(w) => open({ worker: w.metadata.name })}
          isActive={(w) => w.metadata.name === selection.worker}
        />
      ) : (
        <div className={styles.empty}>
          <EmptyState title={t('workers.empty')} description={t('workers.empty_hint')}
            action={<Button onClick={() => navigate('/worker-pools')}>{t('nav.worker_pools')}</Button>} />
        </div>
      )}

      <EntityPanel
        open={panel.open && viewing !== null}
        onClose={closePanel}
        eyebrow={viewing?.workerPool}
        title={viewing?.metadata.name ?? ''}
        status={viewing && <StateBadge state={stateOf(viewing)} namespace="workers.state" />}
        actions={viewing ? actionGroups(viewing) : []}
      >
        {viewing && (
          <div className={styles.form}>
            <Section title={t('workers.placement')}>
              <dl className={styles.details}>
                {/* Пул воркера ссылкой на его панель (#195). */}
                <dt>{t('workers.pool')}</dt>
                <dd className={styles.mono}>
                  <RelatedLink related={workerPool(viewing)} />
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
      </EntityPanel>
    </div>
  );
}
