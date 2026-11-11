import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { IconX } from '@/components/ui/icons';
import { Input } from '@/components/ui/Input';
import { Select } from '@/components/ui/Select';
import type { ActorTemplate, Container } from '@/services/brood/substrate';
import { asJson, blankTemplate, cleaned, copyOf, FIDELITIES, shapeProblem, volumeKind } from './templateDraft';
import { ChipsInput } from './components/ChipsInput';
import { EditorSheet } from './components/EditorSheet';
import { Field, Section } from './components/form';
import { KeyValueEditor } from './components/KeyValueEditor';
import { Segmented } from './components/Segmented';
import {
  bytesToMib,
  clone,
  formatCpu,
  formatMemory,
  fromPairs,
  isShortName,
  mibToBytes,
  toPairs,
  type Pair,
} from './format';
import styles from './substrate.module.scss';

interface TemplateEditorProps {
  /** ID роя: в него уходит создание. */
  project: string;
  /** Имя роя в заголовке листа (#195). */
  swarmName?: string;
  /** The template to copy; null -- start from a blank one. */
  source: ActorTemplate | null;
  open: boolean;
  onClose: () => void;
  onSubmit: (template: ActorTemplate) => Promise<boolean>;
}

/** brood: create an actor template, from scratch or as a copy -- ateapi has no
 * update for templates. The form edits the protojson object in place, the JSON
 * tab shows all of it: fields the form does not cover (wakeup probe, security
 * context, external volumes, golden snapshots) survive either way. */
export function TemplateEditor({
  project,
  swarmName,
  source,
  open,
  onClose,
  onSubmit,
}: TemplateEditorProps) {
  const { t } = useTranslation();
  const [initial] = useState(() => (source ? copyOf(source) : blankTemplate()));
  const [draft, setDraft] = useState(initial);
  const [mode, setMode] = useState<'form' | 'json'>('form');
  const [json, setJson] = useState('');
  const [jsonError, setJsonError] = useState<string | null>(null);
  // Remounts the self-owned key/value editors after the JSON tab or a removed
  // container rewrote what they started from.
  const [revision, setRevision] = useState(0);
  const [saving, setSaving] = useState(false);

  const patch = (change: (next: ActorTemplate) => void) =>
    setDraft((prev) => {
      const next = clone(prev);
      change(next);
      return next;
    });

  const parseJson = (): ActorTemplate | null => {
    try {
      const parsed = JSON.parse(json) as ActorTemplate;
      const problem = shapeProblem(parsed);
      if (problem) throw new Error(t('templates.json_shape', { field: problem }));
      parsed.metadata = { name: String(parsed.metadata?.name ?? '') };
      setJsonError(null);
      return parsed;
    } catch (e) {
      setJsonError(e instanceof Error ? e.message : String(e));
      return null;
    }
  };

  const switchMode = (next: 'form' | 'json') => {
    if (next === mode) return;
    if (next === 'json') {
      setJson(asJson(draft));
      setJsonError(null);
      setMode('json');
      return;
    }
    const parsed = parseJson();
    if (!parsed) return;
    setDraft(parsed);
    setRevision((r) => r + 1);
    setMode('form');
  };

  const current = mode === 'json' ? null : draft;
  const nameValid = isShortName(draft.metadata.name);
  const containersValid =
    (draft.containers?.length ?? 0) > 0 && draft.containers!.every((c) => c.name.trim() && c.image.trim());
  const valid = mode === 'json' ? json.trim() !== '' : nameValid && containersValid;
  const dirty = mode === 'json' ? json !== asJson(initial) : asJson(draft) !== asJson(initial);

  const save = async () => {
    const template = mode === 'json' ? parseJson() : draft;
    if (!template) return;
    setSaving(true);
    try {
      await onSubmit(cleaned(template));
    } finally {
      setSaving(false);
    }
  };

  const fidelity = draft.snapshotConfig?.preferredFidelity ?? '';

  return (
    <EditorSheet
      open={open}
      onClose={onClose}
      size="xl"
      eyebrow={swarmName ?? project}
      title={source ? t('templates.copy_title', { name: source.metadata.name }) : t('templates.create_title')}
      description={t('templates.immutable_hint')}
      onSave={() => void save()}
      saveLabel={t('substrate.create')}
      canSave={valid}
      saving={saving}
      dirty={dirty}
    >
      <div className={styles.form}>
        <div className={styles.toolbar}>
          <Segmented
            value={mode}
            ariaLabel={t('templates.mode')}
            options={[
              { value: 'form', label: t('templates.mode_form') },
              { value: 'json', label: 'JSON' },
            ]}
            onChange={switchMode}
          />
        </div>

        {mode === 'json' && (
          <Field label="JSON" hint={t('templates.json_hint')} error={jsonError ?? undefined}>
            {(id) => (
              <textarea
                id={id}
                className={styles.jsonArea}
                value={json}
                spellCheck={false}
                onChange={(event) => setJson(event.target.value)}
              />
            )}
          </Field>
        )}

        {current && (
          <>
            <Section title={t('templates.basics')}>
              <Input
                name="template-name"
                label={t('substrate.name')}
                value={current.metadata.name}
                autoComplete="off"
                placeholder="claude"
                hint={t('substrate.name_hint')}
                error={current.metadata.name && !nameValid ? t('substrate.name_invalid') : undefined}
                onChange={(event) => patch((d) => (d.metadata.name = event.target.value.trim()))}
              />
            </Section>

            <Section
              title={t('templates.containers')}
              hint={t('templates.containers_hint')}
              action={
                <Button
                  variant="secondary"
                  size="sm"
                  onClick={() => patch((d) => d.containers!.push({ name: '', image: '' }))}
                >
                  {t('templates.add_container')}
                </Button>
              }
            >
              {current.containers!.map((container, index) => (
                <ContainerCard
                  key={`${revision}-${index}`}
                  index={index}
                  container={container}
                  removable={current.containers!.length > 1}
                  onChange={(change) => patch((d) => change(d.containers![index]))}
                  onRemove={() => {
                    patch((d) => d.containers!.splice(index, 1));
                    setRevision((r) => r + 1);
                  }}
                />
              ))}
            </Section>

            <Section
              title={t('templates.volumes')}
              hint={t('templates.volumes_hint')}
              action={
                <Button
                  variant="secondary"
                  size="sm"
                  onClick={() => patch((d) => (d.volumes = [...(d.volumes ?? []), { name: '', durableDir: {} }]))}
                >
                  {t('templates.add_volume')}
                </Button>
              }
            >
              {(current.volumes ?? []).map((volume, index) => (
                <div key={index} className={styles.kvRow}>
                  <input
                    className={`${styles.input} ${styles.mono}`}
                    value={volume.name}
                    placeholder={t('templates.volume_name')}
                    aria-label={t('templates.volume_name')}
                    onChange={(event) => patch((d) => (d.volumes![index].name = event.target.value.trim()))}
                  />
                  <span className={`${styles.muted} ${styles.mono}`}>
                    {t(`templates.volume_kind.${volumeKind(volume)}`, volumeKind(volume))}
                  </span>
                  <button
                    type="button"
                    className={styles.iconButton}
                    aria-label={t('substrate.remove_row')}
                    onClick={() => patch((d) => d.volumes!.splice(index, 1))}
                  >
                    <IconX size={16} />
                  </button>
                </div>
              ))}
            </Section>

            <Section title={t('templates.resources')} hint={t('templates.resources_hint')}>
              <div className={styles.grid2}>
                <Input
                  name="template-cpu"
                  type="number"
                  min={0}
                  step={100}
                  label={t('templates.cpu')}
                  value={current.resources?.limits?.cpuMillicores ?? ''}
                  hint={formatCpu(current.resources?.limits?.cpuMillicores)}
                  onChange={(event) =>
                    patch((d) => {
                      d.resources = { limits: { ...d.resources?.limits, cpuMillicores: event.target.value || undefined } };
                    })
                  }
                />
                <Input
                  name="template-memory"
                  type="number"
                  min={0}
                  step={256}
                  label={t('templates.memory')}
                  value={bytesToMib(current.resources?.limits?.memoryBytes)}
                  hint={formatMemory(current.resources?.limits?.memoryBytes)}
                  onChange={(event) =>
                    patch((d) => {
                      d.resources = { limits: { ...d.resources?.limits, memoryBytes: mibToBytes(event.target.value) } };
                    })
                  }
                />
              </div>
            </Section>

            <Section title={t('substrate.placement')} hint={t('substrate.placement_hint')}>
              <KeyValueEditor
                key={`selector-${revision}`}
                initial={toPairs(current.workerSelector?.matchLabels)}
                onChange={(pairs: Pair[]) =>
                  patch((d) => {
                    const labels = fromPairs(pairs);
                    d.workerSelector = Object.keys(labels).length ? { matchLabels: labels } : undefined;
                  })
                }
                keyPlaceholder={t('substrate.label_key')}
                valuePlaceholder={t('substrate.label_value')}
                addLabel={t('substrate.add_label')}
              />
            </Section>

            <Section title={t('templates.snapshots')} hint={t('templates.snapshots_hint')}>
              <div className={styles.grid2}>
                <Field label={t('templates.fidelity')}>
                  {(id) => (
                    <Select
                      id={id}
                      value={fidelity}
                      fullWidth
                      options={[
                        { value: '', label: t('templates.fidelity_none') },
                        ...FIDELITIES.map((value) => ({
                          value,
                          label: t(`templates.fidelity_${value.replace('SNAPSHOT_FIDELITY_', '').toLowerCase()}`),
                        })),
                      ]}
                      onChange={(value) =>
                        patch((d) => {
                          // Остальное в snapshotConfig (хранилище, золотой
                          // снапшот) не трогаем: форма их не показывает.
                          const rest = { ...d.snapshotConfig };
                          delete rest.preferredFidelity;
                          if (value) d.snapshotConfig = { ...rest, preferredFidelity: value };
                          else d.snapshotConfig = Object.keys(rest).length ? rest : undefined;
                        })
                      }
                    />
                  )}
                </Field>
                <Input
                  name="template-storage"
                  label={t('templates.storage')}
                  value={current.snapshotConfig?.storageLocation ?? ''}
                  placeholder="gs://bucket/prefix/"
                  hint={t('templates.storage_hint')}
                  disabled={!fidelity}
                  onChange={(event) =>
                    patch((d) => {
                      if (d.snapshotConfig) d.snapshotConfig.storageLocation = event.target.value.trim() || undefined;
                    })
                  }
                />
              </div>
            </Section>
          </>
        )}
      </div>
    </EditorSheet>
  );
}

interface ContainerCardProps {
  index: number;
  container: Container;
  removable: boolean;
  onChange: (change: (container: Container) => void) => void;
  onRemove: () => void;
}

function ContainerCard({ index, container, removable, onChange, onRemove }: ContainerCardProps) {
  const { t } = useTranslation();
  // «Обязательно» краснеет, только когда поле покинули пустым, а не сразу.
  const [touched, setTouched] = useState<Record<string, boolean>>({});
  const required = (field: 'name' | 'image', value: string, hint?: string) =>
    value.trim()
      ? { hint }
      : touched[field]
        ? { error: t('substrate.required') }
        : { hint: hint ?? t('substrate.required') };
  const touch = (field: 'name' | 'image') => () => setTouched((prev) => ({ ...prev, [field]: true }));
  return (
    <div className={styles.card}>
      <div className={styles.cardHead}>
        <h4 className={styles.cardTitle}>
          {t('templates.container_n', { n: index + 1 })}
          {container.name ? ` · ${container.name}` : ''}
        </h4>
        {removable && (
          <Button variant="ghost" size="sm" className={styles.dangerText} onClick={onRemove}>
            {t('substrate.delete')}
          </Button>
        )}
      </div>
      <div className={styles.grid2}>
        <Input
          label={t('templates.container_name')}
          value={container.name}
          autoComplete="off"
          {...required('name', container.name)}
          onBlur={touch('name')}
          onChange={(event) => onChange((c) => (c.name = event.target.value.trim()))}
        />
        <Input
          label={t('templates.image')}
          value={container.image}
          autoComplete="off"
          placeholder="registry/image@sha256:…"
          {...required('image', container.image, t('templates.image_hint'))}
          onBlur={touch('image')}
          onChange={(event) => onChange((c) => (c.image = event.target.value.trim()))}
        />
      </div>
      <div className={styles.grid2}>
        <Field label={t('templates.command')} hint={t('substrate.chips_hint')}>
          {(id) => (
            <ChipsInput
              id={id}
              values={container.command ?? []}
              placeholder={t('templates.command_placeholder')}
              onChange={(values) => onChange((c) => (c.command = values.length ? values : undefined))}
            />
          )}
        </Field>
        <Field label={t('templates.args')} hint={t('substrate.chips_hint')}>
          {(id) => (
            <ChipsInput
              id={id}
              values={container.args ?? []}
              placeholder={t('templates.args_placeholder')}
              onChange={(values) => onChange((c) => (c.args = values.length ? values : undefined))}
            />
          )}
        </Field>
      </div>
      <Field label={t('templates.env')} hint={t('templates.env_hint')}>
        {() => (
          <KeyValueEditor
            initial={(container.env ?? []).map((e) => ({ key: e.name, value: e.value ?? '' }))}
            onChange={(pairs) =>
              onChange((c) => (c.env = pairs.length ? pairs.map((p) => ({ name: p.key, value: p.value })) : undefined))
            }
            keyPlaceholder={t('templates.env_name')}
            valuePlaceholder={t('templates.env_value')}
            addLabel={t('templates.add_env')}
          />
        )}
      </Field>
      <Field label={t('templates.mounts')} hint={t('templates.mounts_hint')}>
        {() => (
          <KeyValueEditor
            initial={(container.volumeMounts ?? []).map((m) => ({ key: m.name, value: m.mountPath }))}
            onChange={(pairs) =>
              onChange(
                (c) =>
                  (c.volumeMounts = pairs.length
                    ? pairs.map((p) => ({ name: p.key, mountPath: p.value }))
                    : undefined)
              )
            }
            keyPlaceholder={t('templates.volume_name')}
            valuePlaceholder={t('templates.mount_path')}
            addLabel={t('templates.add_mount')}
          />
        )}
      </Field>
    </div>
  );
}
