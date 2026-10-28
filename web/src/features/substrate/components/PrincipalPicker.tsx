import { useTranslation } from 'react-i18next';
import type { SelectOption } from '@/components/ui/Select';
import styles from '../substrate.module.scss';

/** Политика выбирает реальные варианты из каталога, не произвольную строку.
 * Нативные checkbox сохраняют клавиатурное управление и доступные подписи. */
export function PrincipalPicker({
  id,
  options,
  values,
  onChange,
  disabled = false,
}: {
  id?: string;
  options: SelectOption[];
  values: string[];
  onChange: (values: string[]) => void;
  disabled?: boolean;
}) {
  const { t } = useTranslation();
  return (
    <fieldset
      id={id}
      className={styles.principalPicker}
      disabled={disabled}
      aria-label={t('policy.members')}
    >
      {options.length === 0 && <p className={styles.fieldHint}>{t('access.no_identities')}</p>}
      {options.map((option) => (
        <label key={option.value} className={styles.fieldLabel}>
          <input
            type="checkbox"
            checked={values.includes(option.value)}
            onChange={(event) =>
              onChange(
                event.target.checked
                  ? [...values, option.value]
                  : values.filter((value) => value !== option.value)
              )
            }
          />
          <span title={option.value}>{option.label}</span>
        </label>
      ))}
    </fieldset>
  );
}
