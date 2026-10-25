import { useRef, useState, type KeyboardEvent } from 'react';
import { useTranslation } from 'react-i18next';
import styles from '../substrate.module.scss';

interface ChipsInputProps {
  id?: string;
  values: string[];
  onChange: (values: string[]) => void;
  placeholder: string;
  /** Duplicates make sense in argv, not in a member list. */
  unique?: boolean;
}

/** brood: an ordered list of strings as chips -- command, args, principals.
 * Enter adds what is typed (a value may contain spaces), Backspace on an
 * empty field takes the last chip back for editing. A value still in the
 * field when it loses focus is added too, so Save never drops it. */
export function ChipsInput({ id, values, onChange, placeholder, unique = false }: ChipsInputProps) {
  const { t } = useTranslation();
  const [draft, setDraft] = useState('');
  const inputRef = useRef<HTMLInputElement | null>(null);

  const commit = () => {
    const value = draft.trim();
    if (!value) return;
    if (!(unique && values.includes(value))) onChange([...values, value]);
    setDraft('');
  };

  const onKeyDown = (event: KeyboardEvent<HTMLInputElement>) => {
    if (event.key === 'Enter') {
      event.preventDefault();
      commit();
    } else if (event.key === 'Backspace' && !draft && values.length) {
      event.preventDefault();
      setDraft(values[values.length - 1]);
      onChange(values.slice(0, -1));
    }
  };

  return (
    <div className={styles.chipsField} onClick={() => inputRef.current?.focus()}>
      {values.map((value, index) => (
        <span key={`${value}-${index}`} className={styles.chip}>
          {value}
          <button
            type="button"
            className={styles.chipRemove}
            aria-label={t('substrate.remove_value', { value })}
            onClick={(event) => {
              event.stopPropagation();
              onChange(values.filter((_, i) => i !== index));
            }}
          >
            ×
          </button>
        </span>
      ))}
      <input
        id={id}
        ref={inputRef}
        className={styles.chipsInput}
        value={draft}
        placeholder={values.length ? '' : placeholder}
        // С подписью поля (id) имя даёт она; пример в заглушке -- не имя.
        aria-label={id ? undefined : placeholder}
        onChange={(event) => setDraft(event.target.value)}
        onKeyDown={onKeyDown}
        onBlur={commit}
      />
    </div>
  );
}
