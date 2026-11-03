import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { Input } from '@/components/ui/Input';
import { Select } from '@/components/ui/Select';
import { sessionApi } from '@/services/brood/commands';
import { Field } from './form';
import { replace, useSessionRead } from './useSessionRead';
import styles from '../substrate.module.scss';

const LINES = ['40', '100', '200', '500'];

/** brood: последний вывод экрана Claude (tail). Читается при открытии и по
 * кнопке; автоматического опроса нет. Фильтр оставляет строки с текстом. */
export function ActorOutput({ swarm, name }: { swarm: string; name: string }) {
  const { t, i18n } = useTranslation();
  const [lines, setLines] = useState('100');
  const [grep, setGrep] = useState('');
  const read = useSessionRead<{ text: string; at: Date }>();
  const load = () =>
    read.run(
      async () => ({
        text: await sessionApi.tail(swarm, name, { lines: Number(lines), grep: grep.trim() }),
        at: new Date(),
      }),
      replace
    );

  useEffect(() => {
    void load();
    // Только первое чтение при открытии; дальше — по кнопке.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return (
    <div className={styles.section}>
      <form
        className={styles.consoleControls}
        onSubmit={(event) => {
          event.preventDefault();
          void load();
        }}
      >
        <Field label={t('actors.console.output.lines')}>
          {(id) => (
            <Select
              id={id}
              value={lines}
              options={LINES.map((value) => ({ value, label: value }))}
              onChange={setLines}
            />
          )}
        </Field>
        <Input
          name="actor-output-filter"
          label={t('actors.console.output.filter')}
          hint={t('actors.console.output.filter_hint')}
          value={grep}
          autoComplete="off"
          spellCheck={false}
          maxLength={1024}
          onChange={(event) => setGrep(event.target.value)}
        />
        <Button type="submit" variant="secondary" loading={read.loading}>
          {t('actors.console.refresh')}
        </Button>
      </form>
      {read.error && (
        <p role="alert" className={`${styles.notice} ${styles.operationFailure}`}>
          {read.error}
        </p>
      )}
      {read.data && (
        <>
          <pre className={styles.code} tabIndex={0} aria-label={t('actors.console.output.label', { name })}>
            {read.data.text || t('actors.console.output.empty')}
          </pre>
          <p className={styles.fieldHint}>
            {t('actors.console.read_at', { time: read.data.at.toLocaleTimeString(i18n.language) })}
          </p>
        </>
      )}
    </div>
  );
}
