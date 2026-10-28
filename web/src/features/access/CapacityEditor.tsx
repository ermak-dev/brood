import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { Input } from '@/components/ui/Input';
import type { ShardCapacity } from '@/services/brood/access';
import { EditorSheet } from '@/features/substrate/components/EditorSheet';
import { capacityDraft, capacityFromDraft } from './shardState';
import styles from '@/features/substrate/substrate.module.scss';

const limits = ['max_swarms', 'max_actors', 'max_cpu_millicores', 'max_memory_bytes'] as const;

export function CapacityEditor({
  capacity,
  close,
  submit,
}: {
  capacity: ShardCapacity;
  close: () => void;
  submit: (capacity: ShardCapacity) => Promise<boolean>;
}) {
  const { t } = useTranslation();
  const [draft, setDraft] = useState(() => capacityDraft(capacity));
  const [saving, setSaving] = useState(false);
  const parsed = capacityFromDraft(draft);
  const dirty = JSON.stringify(draft) !== JSON.stringify(capacityDraft(capacity));
  const save = async () => {
    if (!parsed || saving) return;
    setSaving(true);
    try {
      if (await submit(parsed)) close();
    } finally {
      setSaving(false);
    }
  };
  return (
    <EditorSheet
      open
      title={t('access.capacity')}
      onClose={close}
      onSave={() => void save()}
      canSave={!!parsed}
      saving={saving}
      dirty={dirty}
    >
      <form
        className={styles.form}
        onSubmit={(event) => {
          event.preventDefault();
          void save();
        }}
      >
        <p className={styles.sectionHint}>{t('access.capacity_hint')}</p>
        {limits.map((key) => (
          <Input
            key={key}
            label={t(`access.${key}`)}
            inputMode="numeric"
            value={draft[key]}
            disabled={saving}
            onChange={(event) => setDraft({ ...draft, [key]: event.target.value })}
          />
        ))}
        <fieldset disabled={saving}>
          <legend>{t('access.worker_selector')}</legend>
          <p className={styles.fieldHint}>{t('access.worker_selector_hint')}</p>
          {draft.labels.map((label, index) => (
            <div key={index} className={styles.form}>
              <Input
                label={t('access.label_key')}
                value={label.key}
                onChange={(event) =>
                  setDraft({
                    ...draft,
                    labels: draft.labels.map((entry, i) =>
                      i === index ? { ...entry, key: event.target.value } : entry
                    ),
                  })
                }
              />
              <Input
                label={t('access.label_value')}
                value={label.value}
                onChange={(event) =>
                  setDraft({
                    ...draft,
                    labels: draft.labels.map((entry, i) =>
                      i === index ? { ...entry, value: event.target.value } : entry
                    ),
                  })
                }
              />
              <Button
                type="button"
                variant="ghost"
                onClick={() =>
                  setDraft({ ...draft, labels: draft.labels.filter((_, i) => i !== index) })
                }
              >
                {t('substrate.delete')}
              </Button>
            </div>
          ))}
          <Button
            type="button"
            variant="secondary"
            disabled={draft.labels.length >= 10}
            onClick={() =>
              setDraft({ ...draft, labels: [...draft.labels, { key: '', value: '' }] })
            }
          >
            {t('access.add_label')}
          </Button>
        </fieldset>
        {!parsed && <p role="alert">{t('access.capacity_invalid')}</p>}
        <button type="submit" hidden disabled={!parsed || saving}>
          {t('substrate.save')}
        </button>
      </form>
    </EditorSheet>
  );
}
