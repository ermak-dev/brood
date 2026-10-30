import { useCallback, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { EmptyState } from '@/components/ui/EmptyState';
import { Input } from '@/components/ui/Input';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';
import { Select } from '@/components/ui/Select';
import { BroodApiError, errorMessage } from '@/services/brood/http';
import { workerPoolsApi, type WorkerPool } from '@/services/brood/substrate';
import { EditorSheet } from './components/EditorSheet';
import { EntityTable, type Column } from './components/EntityTable';
import { Field, Section } from './components/form';
import { KeyValueEditor } from './components/KeyValueEditor';
import { PageHeader } from './components/PageHeader';
import { DASH, fromPairs, toPairs, type Pair } from './format';
import { useActions, useResource } from './hooks';
import { missingSandboxClasses, newPoolDraft, poolDraft, poolPayload, validatePoolDraft, workerPoolImages, type PoolDraft } from './workerPoolDraft';
import styles from './substrate.module.scss';

const versioned = (pool: WorkerPool) => Boolean(pool.metadata.uid && pool.metadata.resourceVersion);

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

export function WorkerPoolsPage() {
  const { t } = useTranslation();
  const { data, error, loading, refresh } = useResource(workerPoolsApi.list);
  const { confirm } = useActions(refresh);
  const [editing, setEditing] = useState<{ name?: string } | null>(null);
  const remove = (pool: WorkerPool) => confirm({
    title: t('worker_pools.delete_title'),
    message: t('worker_pools.delete_message', { name: pool.metadata.name }),
    confirmText: t('substrate.delete'),
    success: t('worker_pools.deleted', { name: pool.metadata.name }),
    run: () => workerPoolsApi.remove(pool),
  });
  const columns: Column<WorkerPool>[] = [
    { key: 'name', label: t('substrate.name'), primary: true, render: (pool) => pool.metadata.name },
    { key: 'desired', label: t('worker_pools.replicas'), render: (pool) => pool.spec.replicas },
    { key: 'observed', label: t('worker_pools.observed'), render: (pool) => pool.status?.replicas ?? DASH },
    { key: 'ready', label: t('worker_pools.ready'), render: (pool) => pool.status ? (pool.status.readyReplicas ?? 0) : DASH },
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
          onRowClick={(pool) => setEditing({ name: pool.metadata.name })}
          actionsLabel={t('substrate.actions')}
          actions={(pool) => (
            <Button variant="ghost" size="sm" className={styles.dangerText}
              disabled={!versioned(pool)} onClick={() => remove(pool)}>{t('substrate.delete')}</Button>
          )}
        />
      ) : <EmptyState title={t('worker_pools.empty')} description={t('worker_pools.empty_hint')} action={create} />}
      {editing && <PoolEditor key={editing.name ?? ''} name={editing.name} onClose={() => setEditing(null)} refresh={refresh} />}
    </div>
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
