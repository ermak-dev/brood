import { Fragment, useCallback, useState } from 'react';
import type { TFunction } from 'i18next';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { EmptyState } from '@/components/ui/EmptyState';
import { Input } from '@/components/ui/Input';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';
import { Select } from '@/components/ui/Select';
import { BroodApiError, errorMessage } from '@/services/brood/http';
import { proxmoxPoolsApi, workerPoolsApi, type ProxmoxPool, type WorkerPool } from '@/services/brood/substrate';
import { useHeaderRefresh } from '@/hooks/useHeaderRefresh';
import { useAdministrator } from '@/features/access/shared';
import { ConnectionForm } from './ConnectionForm';
import { EditorSheet } from './components/EditorSheet';
import { EntityPanel } from './components/EntityPanel';
import { EntityTable, type Column } from './components/EntityTable';
import { Field, Section } from './components/form';
import { KeyValueEditor } from './components/KeyValueEditor';
import { PageHeader } from './components/PageHeader';
import { DASH, formatCpu, formatMemory, fromPairs, toPairs, type Pair } from './format';
import { useActions, useResource } from './hooks';
import { proxmoxPoolActionGroups, workerPoolActionGroups, type ProxmoxPoolActionId } from './poolActions';
import { missingSandboxClasses, newPoolDraft, poolDraft, poolPayload, validatePoolDraft, versioned, workerPoolImages, type PoolDraft } from './workerPoolDraft';
import styles from './substrate.module.scss';

// У пула нет общей conditions: состояние разрешения — у каждого sandbox class.
function PoolResolution({ pool }: { pool: WorkerPool }) {
  const { t } = useTranslation();
  return <>{pool.spec.sandboxClasses.map(({ name }) => {
    const status = pool.status?.sandboxClasses?.find((entry) => entry.name === name);
    const condition = status?.conditions?.find((entry) => entry.type === 'Resolved');
    return (
      <div key={name}>
        <span>{name}: {condition?.status ?? t('worker_pools.unknown')}</span>
        {status?.configRef && <div className={styles.muted}>{status.configRef.name}</div>}
        {status?.pendingConfigRef && <div>{t('worker_pools.pending_config', { name: status.pendingConfigRef.name })}</div>}
        {condition?.reason && <div>{condition.reason}</div>}
        {condition?.message && <div className={styles.muted}>{condition.message}</div>}
      </div>
    );
  })}</>;
}

/** Метки пула -- то, по чему планировщик сопоставляет его воркеров с
 * workerSelector шаблона. Пул без меток не примет ни одного такого актора,
 * поэтому пустой набор назван словами, а не прочерком. */
function PoolLabels({ pool }: { pool: WorkerPool }) {
  const { t } = useTranslation();
  const labels = Object.entries(pool.metadata.labels ?? {});
  if (!labels.length) return <span className={styles.muted}>{t('worker_pools.no_labels')}</span>;
  return (
    <div className={styles.chips}>
      {labels.map(([key, value]) => (
        <span key={key} className={`${styles.chip} ${styles.chipStatic}`}>{key}={value}</span>
      ))}
    </div>
  );
}

const readyReplicas = (pool: WorkerPool) => (pool.status ? (pool.status.readyReplicas ?? 0) : DASH);
const sandboxOf = (pool: WorkerPool) => pool.spec.sandboxClasses.map((entry) => entry.name).join(', ');

/** Сведения WorkerPool в его панели: реплики, ресурсы воркера, метки и
 * разрешение классов. Правит их редактор, панель только показывает. */
export function WorkerPoolDetails({ pool }: { pool: WorkerPool }) {
  const { t } = useTranslation();
  const resources = pool.spec.template?.resources;
  return (
    <div className={styles.form}>
      <Section title={t('worker_pools.configuration')}>
        <dl className={styles.details}>
          <dt>{t('worker_pools.replicas')}</dt>
          <dd>{pool.spec.replicas}</dd>
          <dt>{t('worker_pools.observed')}</dt>
          <dd>{pool.status?.replicas ?? DASH}</dd>
          <dt>{t('worker_pools.ready')}</dt>
          <dd>{readyReplicas(pool)}</dd>
          <dt>{t('worker_pools.sandbox')}</dt>
          <dd>{sandboxOf(pool) || DASH}</dd>
          <dt>{t('worker_pools.image')}</dt>
          <dd className={styles.mono}>{pool.spec.workerImage || DASH}</dd>
        </dl>
      </Section>
      <Section title={t('worker_pools.resources')}>
        <dl className={styles.details}>
          <dt>{t('worker_pools.request_cpu')}</dt>
          <dd>{resources?.requests?.cpu || DASH}</dd>
          <dt>{t('worker_pools.request_memory')}</dt>
          <dd>{resources?.requests?.memory || DASH}</dd>
          <dt>{t('worker_pools.limit_cpu')}</dt>
          <dd>{resources?.limits?.cpu || DASH}</dd>
          <dt>{t('worker_pools.limit_memory')}</dt>
          <dd>{resources?.limits?.memory || DASH}</dd>
        </dl>
      </Section>
      <Section title={t('worker_pools.labels')} hint={t('worker_pools.labels_hint')}>
        <PoolLabels pool={pool} />
      </Section>
      <Section title={t('worker_pools.resolution')}>
        <PoolResolution pool={pool} />
      </Section>
    </div>
  );
}

/** brood: WorkerPools of the install and its Proxmox pools. A row opens the
 * pool's side panel; editing, deleting and the Proxmox allocation switches
 * live there (#157). */
export function WorkerPoolsPage() {
  const { t } = useTranslation();
  const administrator = useAdministrator();
  const { data, error, loading, refresh } = useResource(workerPoolsApi.list);
  const connections = useResource(proxmoxPoolsApi.list, { headerRefresh: false });
  useHeaderRefresh(connections.refresh);
  const { confirm } = useActions(refresh);
  const [editing, setEditing] = useState<{ name?: string } | null>(null);
  // Имя, а не объект: панель показывает пул после обновления списка, а
  // удалённый пул закрывает её сам.
  const [viewingName, setViewingName] = useState<string | null>(null);
  const viewing = (viewingName !== null && data?.find((pool) => pool.metadata.name === viewingName)) || null;
  const edit = (name: string) => {
    setViewingName(null);
    setEditing({ name });
  };
  const columns: Column<WorkerPool>[] = [
    { key: 'name', label: t('substrate.name'), primary: true, render: (pool) => pool.metadata.name },
    { key: 'desired', label: t('worker_pools.replicas'), render: (pool) => pool.spec.replicas },
    { key: 'observed', label: t('worker_pools.observed'), render: (pool) => pool.status?.replicas ?? DASH },
    { key: 'ready', label: t('worker_pools.ready'), render: readyReplicas },
    { key: 'labels', label: t('worker_pools.labels'), render: (pool) => <PoolLabels pool={pool} /> },
    { key: 'resolution', label: t('worker_pools.resolution'), render: (pool) => <PoolResolution pool={pool} /> },
  ];
  const create = <Button onClick={() => setEditing({})}>{t('worker_pools.create')}</Button>;
  return (
    <div className={styles.page}>
      <PageHeader title={t('worker_pools.title')} actions={create} />
      <p className={styles.notice}>{t('worker_pools.scope_hint')}</p>
      {data?.some((pool) => pool.spec.sandboxClasses.some((entry) => entry.name === 'lxc')) && (
        <p className={styles.notice} role="note">{t('worker_pools.lxc_warning')}</p>
      )}
      {loading ? <LoadingSpinner /> : error ? (
        <EmptyState title={t('substrate.load_failed')} description={error} action={<Button onClick={() => void refresh()}>{t('worker_pools.retry')}</Button>} />
      ) : data?.length ? (
        <EntityTable columns={columns} rows={data} rowKey={(pool) => pool.metadata.uid ?? pool.metadata.name}
          onRowClick={(pool) => setViewingName(pool.metadata.name)}
          isActive={(pool) => pool.metadata.name === viewingName}
        />
      ) : <EmptyState title={t('worker_pools.empty')} description={t('worker_pools.empty_hint')} action={create} />}
      <EntityPanel
        open={viewing !== null}
        onClose={() => setViewingName(null)}
        eyebrow={viewing && sandboxOf(viewing)}
        title={viewing?.metadata.name ?? ''}
        status={viewing && <span>{t('worker_pools.ready')}: {readyReplicas(viewing)} / {viewing.spec.replicas}</span>}
        actions={viewing ? workerPoolActionGroups(viewing, {
          t,
          confirm,
          edit: () => edit(viewing.metadata.name),
          remove: async () => {
            await workerPoolsApi.remove(viewing);
            setViewingName(null);
          },
        }) : []}
      >
        {viewing && <WorkerPoolDetails pool={viewing} />}
      </EntityPanel>
      <ProxmoxConnections items={connections.data} error={connections.error} onRetry={connections.refresh} canAdd={administrator} />
      {editing && <PoolEditor key={editing.name ?? ''} name={editing.name} onClose={() => setEditing(null)} refresh={refresh} />}
    </div>
  );
}

/** Подписанные значения одной ячейки. Значение не рвётся по дефису, длинное --
 * многоточием с полным текстом в title; stacked -- по одному на строку. */
function Facts({ items, stacked }: { items: Array<[string, string]>; stacked?: boolean }) {
  return (
    <dl className={stacked ? `${styles.facts} ${styles.factsStacked}` : styles.facts}>
      {items.map(([term, value]) => (
        <div key={term}><dt>{term}</dt><dd title={value}>{value}</dd></div>
      ))}
    </dl>
  );
}

interface ProxmoxConnectionsProps {
  items: ProxmoxPool[] | null;
  error: string | null;
  onRetry: () => void | Promise<void>;
  /** Форма настройки пула — администратору (#118); без права кнопки нет. */
  canAdd?: boolean;
}

/** Подписанные поля подключения; ни токена, ни его ID здесь нет. */
function connectionFacts(pool: ProxmoxPool, t: TFunction): Array<[string, string]> {
  return [
    [t('proxmox_connections.endpoint'), pool.endpoint],
    [t('proxmox_connections.node'), pool.node],
    [t('proxmox_connections.resource_pool'), pool.resourcePool],
    [t('proxmox_connections.storage'), pool.storage],
    [t('proxmox_connections.network'), pool.network],
  ];
}

/** Резервирование «занято / всего» в единицах человека (CPU/GiB), а не сырые
 * int64 строки; без управляемого пула резервирования нет. */
function capacityFacts(pool: ProxmoxPool, t: TFunction): Array<[string, string]> | null {
  const managed = pool.managedPool;
  if (!managed) return null;
  return [
    [t('proxmox_connections.actors'), `${managed.usedActors} / ${managed.maxActors}`],
    [t('proxmox_connections.cpu'), `${formatCpu(managed.usedCPUMillicores)} / ${formatCpu(managed.cpuMillicores)}`],
    [t('proxmox_connections.memory'), `${formatMemory(managed.usedMemoryBytes)} / ${formatMemory(managed.memoryBytes)}`],
    [t('proxmox_connections.disk'), `${formatMemory(managed.usedDiskBytes)} / ${formatMemory(managed.diskBytes)}`],
  ];
}

function runtimeText(pool: ProxmoxPool, t: TFunction): string {
  return t(pool.runtimeAvailable ? 'proxmox_connections.runtime_available' : 'proxmox_connections.runtime_not_installed');
}

function managedStateText(pool: ProxmoxPool, t: TFunction): string | null {
  const state = pool.managedPool?.state;
  if (state === undefined) return null;
  if (state === 'enabled') return t('proxmox_connections.state_enabled');
  if (state === 'draining') return t('proxmox_connections.state_draining');
  return state;
}

/** Сведения подключения Proxmox в его панели: где оно и сколько занято. */
export function ProxmoxPoolDetails({ pool }: { pool: ProxmoxPool }) {
  const { t } = useTranslation();
  const details = (items: Array<[string, string]>) => (
    <dl className={styles.details}>
      {items.map(([term, value]) => (
        <Fragment key={term}><dt>{term}</dt><dd>{value || DASH}</dd></Fragment>
      ))}
    </dl>
  );
  const capacity = capacityFacts(pool, t);
  return (
    <div className={styles.form}>
      <Section title={t('proxmox_connections.connection')}>{details(connectionFacts(pool, t))}</Section>
      <Section title={t('proxmox_connections.capacity')}>
        {capacity ? details(capacity) : <span className={styles.muted}>{DASH}</span>}
      </Section>
    </div>
  );
}

/** Подключения и резервирования видны даже при отказе Kubernetes; секреты скрыты.
 * Строка открывает панель пула: активация, вывод и включение живут в ней (#157). */
export function ProxmoxConnections({ items, error, onRetry, canAdd }: ProxmoxConnectionsProps) {
  const { t } = useTranslation();
  const [busy, setBusy] = useState(false);
  // Отказ относится к пулу, над которым шло действие: панель другого пула его не показывает.
  const [actionError, setActionError] = useState<{ pool: string; message: string } | null>(null);
  const [adding, setAdding] = useState(false);
  // Имя, а не объект: после действия панель остаётся открытой и показывает
  // новое состояние пула и следующее действие.
  const [viewingName, setViewingName] = useState<string | null>(null);
  const viewing = (viewingName !== null && items?.find((pool) => pool.name === viewingName)) || null;
  const reload = useCallback(async () => { await onRetry(); }, [onRetry]);
  const { confirm } = useActions(reload);
  const perform = async (pool: ProxmoxPool, action: ProxmoxPoolActionId) => {
    setBusy(true);
    setActionError(null);
    try {
      return await proxmoxPoolsApi[action](pool.name);
    } catch (error) {
      setActionError({ pool: pool.name, message: errorMessage(error) });
      throw error;
    } finally {
      setBusy(false);
    }
  };
  if (!canAdd && !error && !items?.length) return null;
  // Строка только открывает панель: поля подключения -- одна подписанная
  // ячейка, учётные данные -- в состоянии. У значения ячейки один корень:
  // на телефоне ячейка -- сетка «подпись | значение».
  const columns: Column<ProxmoxPool>[] = [
    { key: 'name', label: t('substrate.name'), primary: true, render: (pool) => pool.name },
    { key: 'connection', label: t('proxmox_connections.connection'), render: (pool) => <Facts items={connectionFacts(pool, t)} /> },
    { key: 'state', label: t('substrate.state'), render: (pool) => (
      <div>
        <div>{runtimeText(pool, t)}</div>
        {pool.managedPool && <div>{managedStateText(pool, t)}</div>}
      </div>
    ) },
    { key: 'capacity', label: t('proxmox_connections.capacity'), render: (pool) => {
      const capacity = capacityFacts(pool, t);
      return capacity ? <Facts stacked items={capacity} /> : DASH;
    } },
  ];
  const used = viewing?.managedPool;
  return (
    <section className={styles.section} aria-label={t('proxmox_connections.title')}>
      <div className={styles.sectionHead}>
        <h2 className={styles.sectionTitle}>{t('proxmox_connections.title')}</h2>
        {canAdd && <Button size="sm" onClick={() => setAdding(true)}>{t('proxmox_connections.add')}</Button>}
      </div>
      {adding && <ConnectionForm onClose={() => setAdding(false)} refresh={reload} />}
      {error ? (
        <div className={styles.toolbar}>
          <p className={styles.fieldError} role="alert">{t('proxmox_connections.load_failed')}</p>
          <Button variant="secondary" size="sm" onClick={onRetry}>{t('worker_pools.retry')}</Button>
        </div>
      ) : (
        <>
          {items?.some((pool) => pool.runtimeAvailable || pool.managedPool) &&
            <p className={styles.sectionHint}>{t('proxmox_connections.profile_hint')}</p>
          }
          {actionError && !viewing && <p className={styles.fieldError} role="alert">{actionError.message}</p>}
          <EntityTable columns={columns} rows={items ?? []} rowKey={(pool) => pool.name}
            onRowClick={(pool) => setViewingName(pool.name)}
            isActive={(pool) => pool.name === viewingName} />
        </>
      )}
      <EntityPanel
        open={viewing !== null}
        onClose={() => setViewingName(null)}
        eyebrow={t('proxmox_connections.title')}
        title={viewing?.name ?? ''}
        status={viewing && (
          <>
            <span>{runtimeText(viewing, t)}</span>
            {used && <span>{managedStateText(viewing, t)}</span>}
            {used && <span>{t('proxmox_connections.actors')}: {used.usedActors} / {used.maxActors}</span>}
          </>
        )}
        actions={viewing ? proxmoxPoolActionGroups(viewing, {
          t, confirm, pending: busy, perform: (action) => perform(viewing, action),
        }) : []}
      >
        {viewing && actionError?.pool === viewing.name && (
          <p className={styles.fieldError} role="alert">{actionError.message}</p>
        )}
        {viewing && <ProxmoxPoolDetails pool={viewing} />}
      </EntityPanel>
    </section>
  );
}

interface EditorProps {
  name?: string;
  onClose: () => void;
  refresh: () => Promise<void>;
}

function PoolEditor({ name, onClose, refresh }: EditorProps) {
  const { t } = useTranslation();
  const load = useCallback(async () => {
    if (name !== undefined) {
      const source = await workerPoolsApi.get(name);
      return { source, draft: poolDraft(source), images: {}, missing: [] as string[] };
    }
    const defaults = await workerPoolsApi.defaults();
    const images = workerPoolImages(defaults);
    const sandboxClass = Object.keys(images)[0] ?? '';
    return {
      source: null,
      draft: newPoolDraft(images[sandboxClass] ?? '', sandboxClass),
      images,
      missing: missingSandboxClasses(defaults),
    };
  }, [name]);
  const resource = useResource(load, { headerRefresh: false });
  if (resource.data) return <PoolForm source={resource.data.source} initial={resource.data.draft} images={resource.data.images} missing={resource.data.missing} onClose={onClose} refresh={refresh} />;
  return (
    <EditorSheet open onClose={onClose} title={name ?? t('worker_pools.create')}>
      {resource.error ? <EmptyState title={t('substrate.load_failed')} description={resource.error}
        action={<Button onClick={() => void resource.refresh()}>{t('worker_pools.retry')}</Button>} /> : <LoadingSpinner />}
    </EditorSheet>
  );
}

interface FormProps extends Omit<EditorProps, 'name'> {
  source: WorkerPool | null;
  initial: PoolDraft;
  images: Record<string, string>;
  /** Поддерживаемые классы без объявленного образа: их называем, а не прячем. */
  missing: string[];
}

function PoolForm({ source: loadedSource, initial: loadedDraft, images, missing, onClose, refresh }: FormProps) {
  const { t } = useTranslation();
  // Обновление списка из шапки не меняет предусловие открытого черновика.
  const [{ source, initial }] = useState(() => ({ source: loadedSource, initial: loadedDraft }));
  const [draft, setDraft] = useState(initial);
  const [touched, setTouched] = useState<Partial<Record<keyof PoolDraft, boolean>>>({});
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  const { run } = useActions(refresh);
  const errors = validatePoolDraft(draft);
  const dirty = JSON.stringify(initial) !== JSON.stringify(draft);
  const save = async () => {
    if (saving || Object.keys(errors).length || (source && !versioned(source))) return;
    setSaving(true);
    setSaveError(null);
    const ok = await run(async () => {
      try {
        const payload = poolPayload(source, draft);
        return await (source ? workerPoolsApi.update(payload) : workerPoolsApi.create(payload));
      } catch (error) {
        setSaveError(error instanceof BroodApiError && error.status === 409
          ? t('worker_pools.conflict') : errorMessage(error));
        throw error;
      }
    }, t('worker_pools.saved', { name: draft.name }));
    setSaving(false);
    if (ok) onClose();
  };
  // Текстовые поля черновика; метки правит KeyValueEditor, а не этот помощник.
  type TextField = { [K in keyof PoolDraft]: PoolDraft[K] extends string ? K : never }[keyof PoolDraft];
  const input = (field: TextField, label: string, hint?: string) => (
    <Input label={label} value={draft[field]} disabled={saving || field === 'workerImage' || (field === 'name' && source !== null)}
      hint={hint} error={touched[field] && errors[field] ? t(`worker_pools.validation.${errors[field]}`) : undefined}
      onBlur={() => setTouched((current) => ({ ...current, [field]: true }))}
      onChange={(event) => setDraft({ ...draft, [field]: event.target.value })}
      {...(field === 'replicas' ? { type: 'number', min: 0, max: 2147483647, step: 1 } : {})} />
  );
  return (
    <EditorSheet open onClose={onClose} title={source?.metadata.name ?? t('worker_pools.create')}
      description={t('worker_pools.change_warning')} dirty={dirty} saving={saving}
      onSave={() => void save()} canSave={!saving && !Object.keys(errors).length && (!source || (versioned(source) && dirty))}>
      <div className={styles.form}>
        {saveError && <p className={styles.fieldError} role="alert">{saveError}</p>}
        {source && !versioned(source) && <p role="alert">{t('worker_pools.missing_version')}</p>}
        <Section title={t('worker_pools.configuration')}>
          {input('name', t('substrate.name'))}
          {input('replicas', t('worker_pools.replicas'))}
          {source ? (
            <Input label={t('worker_pools.sandbox')} disabled
              value={source.spec.sandboxClasses.map((entry) => entry.name).join(', ')} />
          ) : (
            <Field label={t('worker_pools.sandbox')}
              error={errors.sandboxClass ? t('worker_pools.validation.image') : undefined}>
              {(id) => <Select id={id} value={draft.sandboxClass} disabled={saving}
                ariaLabel={t('worker_pools.sandbox')} ariaInvalid={Boolean(errors.sandboxClass)}
                options={Object.keys(images).map((value) => ({ value, label: value }))}
                onChange={(sandboxClass) => setDraft({
                  ...draft, sandboxClass, workerImage: images[sandboxClass],
                })} />}
            </Field>
          )}
          {draft.sandboxClass === 'lxc' && <p className={styles.notice} role="note">{t('worker_pools.lxc_warning')}</p>}
          {!source && missing.length > 0 && (
            <p className={styles.notice} role="note">
              {t('worker_pools.not_installed', { classes: missing.join(', ') })}
              {missing.includes('lxc') && ` ${t('worker_pools.lxc_install_hint')}`}
            </p>
          )}
          {input('workerImage', t('worker_pools.image'), t('worker_pools.image_hint'))}
        </Section>
        <Section title={t('worker_pools.labels')} hint={t('worker_pools.labels_hint')}>
          <KeyValueEditor
            initial={toPairs(initial.labels)}
            onChange={(pairs: Pair[]) => setDraft((current) => ({ ...current, labels: fromPairs(pairs) }))}
            keyPlaceholder={t('substrate.label_key')}
            valuePlaceholder={t('substrate.label_value')}
            addLabel={t('substrate.add_label')}
            disabled={saving}
          />
          {errors.labels && <p className={styles.fieldError} role="alert">{t('worker_pools.validation.label')}</p>}
        </Section>
        <Section title={t('worker_pools.resources')} hint={t('worker_pools.resources_hint')}>
          <div className={styles.grid2}>
            {input('requestCpu', t('worker_pools.request_cpu'))}
            {input('requestMemory', t('worker_pools.request_memory'))}
            {input('limitCpu', t('worker_pools.limit_cpu'))}
            {input('limitMemory', t('worker_pools.limit_memory'))}
          </div>
        </Section>
        {source && <Section title={t('worker_pools.resolution')}><PoolResolution pool={source} /></Section>}
      </div>
    </EditorSheet>
  );
}
