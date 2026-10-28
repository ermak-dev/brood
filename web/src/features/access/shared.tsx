import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { useAuthStore, useNotificationStore } from '@/stores';
import { useHeaderRefresh } from '@/hooks/useHeaderRefresh';
import { errorMessage } from '@/services/brood/http';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';
import { Button } from '@/components/ui/Button';
import { Input } from '@/components/ui/Input';
import { Select, type SelectOption } from '@/components/ui/Select';
import { EditorSheet } from '@/features/substrate/components/EditorSheet';
import { Field } from '@/features/substrate/components/form';
import styles from '@/features/substrate/substrate.module.scss';
import { currentResource, validEditorField, type ResourceSnapshot } from './state';

export function useAdministrator() {
  return useAuthStore((state) => 'isSuperuser' in state && state.isSuperuser === true);
}

export function useAccessResource<T>(
  load: () => Promise<T>,
  { headerRefresh = true }: { headerRefresh?: boolean } = {}
) {
  const [snapshot, setSnapshot] = useState<ResourceSnapshot<T>>({
    source: load,
    data: null,
    error: '',
    loading: true,
  });
  const latest = useRef(0);
  const activeSource = useRef<(() => Promise<T>) | null>(load);
  const refresh = useCallback(async () => {
    if (activeSource.current !== load) return;
    const version = ++latest.current;
    setSnapshot({ source: load, data: null, error: '', loading: true });
    try {
      const data = await load();
      if (version === latest.current) {
        setSnapshot({ source: load, data, error: '', loading: false });
      }
    } catch (error) {
      if (version === latest.current) {
        setSnapshot({ source: load, data: null, error: errorMessage(error), loading: false });
      }
    }
  }, [load]);
  useEffect(() => {
    activeSource.current = load;
    void refresh();
    return () => {
      activeSource.current = null;
      latest.current += 1;
    };
  }, [load, refresh]);
  useHeaderRefresh(refresh, headerRefresh);
  return { ...currentResource(snapshot, load), refresh };
}

export function LoadState({
  loading,
  error,
  retry,
  children,
}: {
  loading: boolean;
  error: string;
  retry: () => Promise<void>;
  children: ReactNode;
}) {
  const { t } = useTranslation();
  if (loading) return <LoadingSpinner />;
  if (error)
    return (
      <div role="alert">
        <p>{error}</p>
        <Button onClick={() => void retry()}>{t('access.refresh')}</Button>
      </div>
    );
  return children;
}

export interface EditorField {
  name: string;
  label: string;
  type?: 'text' | 'password' | 'checkbox' | 'select';
  required?: boolean;
  /** Варианты поля типа select: имя пользователя выбирается из списка, а не
   * вводится руками -- опечатку иначе ловит только отказ сервера. */
  options?: SelectOption[];
  hint?: string;
  /** Пустой список вариантов: выбирать нечего, и редактор говорит, почему. */
  empty?: string;
}

export function AccessEditor({
  title,
  description,
  fields,
  submit,
  close,
}: {
  title: string;
  description?: string;
  fields: EditorField[];
  submit: (values: Record<string, string>) => Promise<boolean>;
  close: () => void;
}) {
  const { t } = useTranslation();
  const [values, setValues] = useState<Record<string, string>>({});
  const [saving, setSaving] = useState(false);
  const valid = fields.every((field) => validEditorField(field, values[field.name]));
  const save = async () => {
    if (!valid || saving) return;
    setSaving(true);
    try {
      if (await submit(values)) close();
    } finally {
      setSaving(false);
    }
  };
  return (
    <EditorSheet
      open
      title={title}
      description={description}
      onClose={close}
      onSave={() => void save()}
      saveLabel={t('substrate.save')}
      canSave={valid}
      saving={saving}
      dirty={Object.values(values).some(Boolean)}
    >
      <form
        className={styles.form}
        onSubmit={(event) => {
          event.preventDefault();
          void save();
        }}
      >
        {fields.map((field, index) =>
          field.type === 'select' ? (
            <Field key={field.name} label={field.label} hint={field.hint}>
              {(id) =>
                field.options?.length ? (
                  <Select
                    id={id}
                    fullWidth
                    disabled={saving}
                    value={values[field.name] ?? ''}
                    placeholder={field.label}
                    options={field.options}
                    onChange={(value) => setValues({ ...values, [field.name]: value })}
                  />
                ) : (
                  <p className={styles.fieldHint}>{field.empty}</p>
                )
              }
            </Field>
          ) : field.type === 'checkbox' ? (
            <label key={field.name} className={styles.fieldLabel}>
              <input
                type="checkbox"
                checked={values[field.name] === 'true'}
                disabled={saving}
                onChange={(event) =>
                  setValues({ ...values, [field.name]: String(event.target.checked) })
                }
              />
              {field.label}
            </label>
          ) : (
            <Input
              key={field.name}
              name={field.name}
              type={field.type ?? 'text'}
              label={field.label}
              required={field.required}
              value={values[field.name] ?? ''}
              autoFocus={index === 0}
              autoComplete={field.type === 'password' ? 'new-password' : 'off'}
              disabled={saving}
              onChange={(event) => setValues({ ...values, [field.name]: event.target.value })}
            />
          )
        )}
        <button type="submit" hidden disabled={!valid || saving}>
          {t('substrate.save')}
        </button>
      </form>
    </EditorSheet>
  );
}

export function useAccessActions(refresh: () => Promise<void>) {
  const { t } = useTranslation();
  const notify = useNotificationStore((state) => state.showNotification);
  const showConfirmation = useNotificationStore((state) => state.showConfirmation);
  const [busy, setBusy] = useState(false);
  const lock = useRef(false);
  const run = async (mutation: () => Promise<unknown>) => {
    if (lock.current) return false;
    lock.current = true;
    setBusy(true);
    try {
      await mutation();
      notify(t('access.saved'), 'success');
      await refresh();
      return true;
    } catch (error) {
      notify(errorMessage(error), 'error');
      // Сбой провижининга всё равно может оставить строку роя в состоянии error.
      await refresh();
      return false;
    } finally {
      lock.current = false;
      setBusy(false);
    }
  };
  // Текст подтверждения называет последствие именно этого действия: один
  // текст на все действия обещал удаление проекта при разблокировке.
  const confirm = (
    name: string,
    mutation: () => Promise<unknown>,
    title = t('substrate.delete'),
    message = t('access.confirm', { name })
  ) => {
    showConfirmation({
      title,
      message,
      confirmText: title,
      cancelText: t('common.cancel'),
      variant: 'danger',
      onConfirm: async () => {
        await run(mutation);
      },
    });
  };
  return { run, confirm, busy };
}
