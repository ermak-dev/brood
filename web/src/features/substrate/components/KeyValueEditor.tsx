import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { IconX } from '@/components/ui/icons';
import type { Pair } from '../format';
import styles from '../substrate.module.scss';

interface KeyValueEditorProps {
  /** Rows to start from; the editor owns them afterwards (remount to reset). */
  initial: Pair[];
  onChange: (pairs: Pair[]) => void;
  keyPlaceholder: string;
  valuePlaceholder: string;
  addLabel: string;
  disabled?: boolean;
}

/** brood: editable key/value rows -- labels, env vars, volume mounts. The rows
 * live here, so a fresh row without a key survives typing even when the caller
 * keeps a map (fromPairs drops it there). */
export function KeyValueEditor({
  initial,
  onChange,
  keyPlaceholder,
  valuePlaceholder,
  addLabel,
  disabled = false,
}: KeyValueEditorProps) {
  const { t } = useTranslation();
  const [pairs, setPairs] = useState(initial);
  const update = (next: Pair[]) => {
    setPairs(next);
    onChange(next);
  };
  const set = (index: number, patch: Partial<Pair>) =>
    update(pairs.map((pair, i) => (i === index ? { ...pair, ...patch } : pair)));

  return (
    <div className={styles.kvRows}>
      {pairs.map((pair, index) => (
        <div key={index} className={styles.kvRow}>
          <input
            className={`${styles.input} ${styles.mono}`}
            value={pair.key}
            placeholder={keyPlaceholder}
            aria-label={keyPlaceholder}
            disabled={disabled}
            onChange={(event) => set(index, { key: event.target.value })}
          />
          <input
            className={`${styles.input} ${styles.mono}`}
            value={pair.value}
            placeholder={valuePlaceholder}
            aria-label={valuePlaceholder}
            disabled={disabled}
            onChange={(event) => set(index, { value: event.target.value })}
          />
          <button
            type="button"
            className={styles.iconButton}
            aria-label={t('substrate.remove_row')}
            title={t('substrate.remove_row')}
            disabled={disabled}
            onClick={() => update(pairs.filter((_, i) => i !== index))}
          >
            <IconX size={16} />
          </button>
        </div>
      ))}
      {!disabled && (
        <Button
          variant="secondary"
          size="sm"
          className={styles.addButton}
          onClick={() => update([...pairs, { key: '', value: '' }])}
        >
          {addLabel}
        </Button>
      )}
    </div>
  );
}
