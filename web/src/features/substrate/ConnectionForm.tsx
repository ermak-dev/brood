import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Input } from '@/components/ui/Input';
import { BroodApiError, errorMessage } from '@/services/brood/http';
import { proxmoxConnectionsApi } from '@/services/brood/substrate';
import { connectionPayload, newConnectionDraft, validateConnectionDraft, type ConnectionDraft } from './connectionDraft';
import { useActions } from './hooks';
import { EditorSheet } from './components/EditorSheet';
import { Field, Section } from './components/form';
import styles from './substrate.module.scss';

interface ConnectionFormProps {
  onClose: () => void;
  refresh: () => Promise<void>;
}

export function ConnectionForm({ onClose, refresh }: ConnectionFormProps) {
  const { t } = useTranslation();
  const [draft, setDraft] = useState(newConnectionDraft);
  const [touched, setTouched] = useState<Partial<Record<keyof ConnectionDraft, boolean>>>({});
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  const { run } = useActions(refresh);
  const errors = validateConnectionDraft(draft);
  const valid = Object.keys(errors).length === 0;
  const dirty = Object.values(draft).some(Boolean);
  const set = (field: keyof ConnectionDraft, value: string) =>
    setDraft((current) => ({ ...current, [field]: value }));
  const save = async () => {
    if (saving || !valid) return;
    setSaving(true);
    setSaveError(null);
    try {
      const ok = await run(async () => {
        try {
          return await proxmoxConnectionsApi.create(connectionPayload(draft));
        } catch (error) {
          setSaveError(error instanceof BroodApiError && error.status === 409
            ? t('proxmox_connections.duplicate_name') : errorMessage(error));
          throw error;
        }
      }, t('proxmox_connections.created', { name: draft.name }));
      if (ok) {
        setDraft(newConnectionDraft());
        onClose();
      }
    } finally {
      setSaving(false);
    }
  };
  const field = (key: keyof ConnectionDraft, numeric = false) => (
    <Input key={key} name={`connection-${key}`} label={t(`proxmox_connections.fields.${key}`)}
      value={draft[key]} disabled={saving} autoComplete={key === 'tokenSecret' ? 'new-password' : 'off'}
      type={key === 'tokenSecret' ? 'password' : numeric ? 'number' : 'text'}
      {...(numeric ? { min: 1, step: 1 } : {})}
      error={touched[key] && errors[key] ? t(`proxmox_connections.validation.${errors[key]}`) : undefined}
      onBlur={() => setTouched((current) => ({ ...current, [key]: true }))}
      onChange={(event) => set(key, event.target.value)} />
  );
  return (
    <EditorSheet open onClose={onClose} title={t('proxmox_connections.form_title')} size="lg"
      description={t('proxmox_connections.form_hint')} dirty={dirty}
      onSave={() => void save()} saveLabel={t('proxmox_connections.create')}
      canSave={valid && !saving} saving={saving}>
      <div className={styles.form}>
        {saveError && <p className={styles.fieldError} role="alert">{saveError}</p>}
        <Section title={t('proxmox_connections.form_section')}>
          {field('name')}
          {field('endpoint')}
          <div className={styles.grid2}>{field('tokenID')}{field('tokenSecret')}</div>
          <Field label={t('proxmox_connections.fields.caPEM')} hint={t('proxmox_connections.ca_hint')}
            error={touched.caPEM && errors.caPEM ? t(`proxmox_connections.validation.${errors.caPEM}`) : undefined}>
            {(id) => <textarea id={id} name="connection-caPEM" className={`${styles.jsonArea} ${styles.certificateArea}`}
              rows={6} spellCheck={false} autoComplete="off" value={draft.caPEM} disabled={saving}
              onBlur={() => setTouched((current) => ({ ...current, caPEM: true }))}
              onChange={(event) => set('caPEM', event.target.value)} />}
          </Field>
        </Section>
        <Section title={t('proxmox_connections.placement_section')}>
          <div className={styles.grid2}>
            {field('node')}{field('resourcePool')}{field('storage')}{field('network')}
          </div>
        </Section>
        <Section title={t('proxmox_connections.runtime_section')} hint={t('proxmox_connections.runtime_hint')}>
          {field('template')}{field('templateDigest')}
          <div className={styles.grid2}>
            {field('actors', true)}{field('cpu', true)}{field('memoryGiB', true)}{field('diskGiB', true)}
          </div>
        </Section>
      </div>
    </EditorSheet>
  );
}
