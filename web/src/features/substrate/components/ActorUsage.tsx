import { Fragment, useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { Select } from '@/components/ui/Select';
import { sessionApi, type UsageReport } from '@/services/brood/commands';
import { EntityTable } from './EntityTable';
import { Field } from './form';
import { replace, useSessionRead } from './useSessionRead';
import styles from '../substrate.module.scss';

const DAYS = ['1', '7', '14', '30', '90'];

/** brood: расход токенов сессии по transcript за выбранный период. Это не
 * биллинг: удалённые transcript не учитываются, неполный скан отмечен. */
export function ActorUsage({ swarm, name }: { swarm: string; name: string }) {
  const { t, i18n } = useTranslation();
  const [days, setDays] = useState('14');
  const read = useSessionRead<UsageReport>();
  const load = (period: string) =>
    read.run(() => sessionApi.stat(swarm, name, Number(period)), replace);
  const number = (value: number) => value.toLocaleString(i18n.language);

  useEffect(() => {
    void load(days);
    // Период меняется в обработчике выбора; здесь — только первое чтение.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const usage = read.data;
  const totals = usage
    ? [
        ['input', usage.input],
        ['output', usage.output],
        ['cache_write', usage.cacheWrite],
        ['cache_read', usage.cacheRead],
      ]
    : [];

  return (
    <div className={styles.section}>
      <div className={styles.consoleControls}>
        <Field label={t('actors.console.usage.period')}>
          {(id) => (
            <Select
              id={id}
              value={days}
              options={DAYS.map((value) => ({
                value,
                label: t('actors.console.usage.days', { count: Number(value) }),
              }))}
              onChange={(value) => {
                setDays(value);
                void load(value);
              }}
            />
          )}
        </Field>
        <Button type="button" variant="secondary" loading={read.loading} onClick={() => void load(days)}>
          {t('actors.console.refresh')}
        </Button>
      </div>
      {read.error && (
        <p role="alert" className={`${styles.notice} ${styles.operationFailure}`}>
          {read.error}
        </p>
      )}
      {usage && (
        <>
          {usage.partial && <p className={styles.notice}>{t('actors.console.usage.partial')}</p>}
          <dl className={styles.details}>
            {totals.map(([key, value]) => (
              <Fragment key={key}>
                <dt>{t(`actors.console.usage.${key}`)}</dt>
                <dd className={styles.mono}>{number(value as number)}</dd>
              </Fragment>
            ))}
          </dl>
          {usage.perDay.length > 0 && (
            <EntityTable
              rows={usage.perDay}
              rowKey={(row) => row.day}
              columns={[
                { key: 'day', label: t('actors.console.usage.day'), primary: true, render: (row) => row.day },
                { key: 'input', label: t('actors.console.usage.input'), render: (row) => number(row.input) },
                { key: 'output', label: t('actors.console.usage.output'), render: (row) => number(row.output) },
                {
                  key: 'cache_write',
                  label: t('actors.console.usage.cache_write'),
                  render: (row) => number(row.cacheWrite),
                },
                {
                  key: 'cache_read',
                  label: t('actors.console.usage.cache_read'),
                  render: (row) => number(row.cacheRead),
                },
              ]}
            />
          )}
          <p className={styles.fieldHint}>{t('actors.console.usage.scope')}</p>
        </>
      )}
    </div>
  );
}
