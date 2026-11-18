import { useCallback, useId, useRef, useState, type FormEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import { Button } from '@/components/ui/Button';
import { LoadState } from '@/features/access/shared';
import { Section } from '@/features/substrate/components/form';
import { useResource } from '@/features/substrate/hooks';
import { evalApi, validEvalSelection, type SwarmEval } from '@/services/brood/eval';
import { errorMessage } from '@/services/brood/http';
import { pipelinesApi } from '@/services/brood/pipelines';
import { swarmPassComparisonPath, swarmPassPath } from './routes';
import styles from '@/features/substrate/substrate.module.scss';
import local from './SwarmEvalPanel.module.scss';

export interface SwarmEvalPanelProps {
  swarmId: string;
  projectId: string;
  /** undefined — статус ещё не прочитан; только null означает отсутствие binding. */
  binding: SwarmEval | null | undefined;
  error: string;
  canBind: boolean;
  canStart: boolean;
  changed: () => Promise<void>;
  busyChanged: (busy: boolean) => void;
}

/** Два явных этапа: сначала metadata binding, затем существующие managed-акторы
 * и роли. Ни одного provider-действия или изменения закреплённого выбора. */
export function SwarmEvalPanel({
  swarmId,
  projectId,
  binding,
  error,
  canBind,
  canStart,
  changed,
  busyChanged,
}: SwarmEvalPanelProps) {
  const { t } = useTranslation();
  const id = useId();
  const [pending, setPending] = useState(false);
  const [refusal, setRefusal] = useState('');
  const [started, setStarted] = useState(false);
  const lock = useRef(false);
  const load = useCallback(() => pipelinesApi.listDefinitions(projectId), [projectId]);
  const catalog = useResource(binding === null && canBind ? load : null, { headerRefresh: false });
  const mutate = async (operation: () => Promise<unknown>, start = false) => {
    if (lock.current) return;
    lock.current = true;
    setPending(true);
    busyChanged(true);
    setRefusal('');
    try {
      await operation();
      if (start) setStarted(true);
    } catch (failure) {
      setRefusal(errorMessage(failure));
    } finally {
      // Перечитывание входит в busy-интервал: устаревшая idle не даёт второй start.
      try {
        await changed();
      } finally {
        lock.current = false;
        setPending(false);
        busyChanged(false);
      }
    }
  };
  const bind = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (pending || lock.current || !canBind || binding !== null) return;
    const form = event.currentTarget;
    const value = (name: string) =>
      (form.elements.namedItem(name) as HTMLInputElement | HTMLSelectElement).value;
    const selection = {
      referenceSwarmId: value('referenceSwarmId'),
      referencePass: Number(value('referencePass')),
      definition: value('definition'),
      model: value('model'),
    };
    if (!validEvalSelection(selection)) {
      setRefusal(t('swarm_eval.invalid'));
      return;
    }
    void mutate(() => evalApi.bind(swarmId, selection));
  };
  const field = (name: string, type = 'text') => (
    <label className={styles.field} htmlFor={`${id}-${name}`}>
      <span className={styles.fieldLabel}>{t(`swarm_eval.${name}`)}</span>
      <input
        id={`${id}-${name}`}
        name={name}
        type={type}
        className="input"
        required
        disabled={pending}
        autoComplete="off"
        defaultValue=""
        {...(type === 'number' ? { min: 1, step: 1 } : {})}
        {...(name === 'model' ? { maxLength: 256, pattern: '[\\x21-\\x7e]{1,256}' } : {})}
      />
    </label>
  );
  if (binding === null && !canBind && !error) return null;
  return (
    <div data-eval="">
      <Section title={t('swarm_eval.title')} hint={t('swarm_eval.preconditions')}>
        {error ? (
          <LoadState loading={false} error={error} retry={changed}>
            {null}
          </LoadState>
        ) : binding === undefined ? (
          <p>{t('common.loading')}</p>
        ) : binding ? (
          <>
            <p className={styles.notice}>
              <strong>{t('swarm_eval.nonProduction')}</strong>
            </p>
            <p>{t('swarm_eval.configure')}</p>
            <dl className={local.facts}>
              <dt>{t('swarm_eval.referenceSwarmId')}</dt>
              <dd>{binding.referenceSwarmId}</dd>
              <dt>{t('swarm_eval.referencePass')}</dt>
              <dd>{binding.referencePass}</dd>
              <dt>{t('swarm_eval.definition')}</dt>
              <dd>
                {binding.definition.name} · {binding.definition.version} ·{' '}
                {binding.definition.revision}
              </dd>
              <dt>{t('swarm_eval.model')}</dt>
              <dd>{binding.model}</dd>
              <dt>{t('swarm_eval.origin')}</dt>
              <dd>{binding.source.origin}</dd>
              <dt>{t('swarm_eval.commit')}</dt>
              <dd>{binding.source.commit}</dd>
              <dt>{t('swarm_eval.goal')}</dt>
              <dd>
                <pre>{binding.goal}</pre>
              </dd>
              <dt>{t('swarm_eval.execution')}</dt>
              <dd>{binding.execution}</dd>
            </dl>
            <p className={local.links}>
              <Link to={swarmPassPath(binding.referenceSwarmId, binding.referencePass)}>
                {t('swarm_eval.trace')}
              </Link>
              <Link to={swarmPassComparisonPath(binding.referenceSwarmId, binding.referencePass)}>
                {t('swarm_eval.compare')}
              </Link>
            </p>
            <h3>{t('swarm_eval.replay')}</h3>
            {binding.replay ? (
              <dl className={local.facts}>
                <dt>{t('swarm_eval.request')}</dt>
                <dd>{binding.replay.requestId}</dd>
                <dt>{t('swarm_eval.state')}</dt>
                <dd>{t(`swarm_eval.${binding.replay.state}`)}</dd>
                <dt>{t('swarm_eval.reason')}</dt>
                <dd>
                  <pre>{binding.replay.reason}</pre>
                </dd>
                <dt>{t('swarm_eval.interrupted')}</dt>
                <dd>{t(binding.replay.interruptedChildren ? 'common.yes' : 'common.no')}</dd>
              </dl>
            ) : (
              <p>{t('swarm_eval.noReplay')}</p>
            )}
            {canStart && !started && (
              <Button
                data-eval-start=""
                disabled={pending}
                onClick={() => void mutate(() => evalApi.start(swarmId), true)}
              >
                {t('swarm_eval.start')}
              </Button>
            )}
          </>
        ) : canBind ? (
          <form onSubmit={bind} className={local.form}>
            <p>{t('swarm_eval.bindHint')}</p>
            {field('referenceSwarmId')}
            {field('referencePass', 'number')}
            <label className={styles.field} htmlFor={`${id}-definition`}>
              <span className={styles.fieldLabel}>{t('swarm_eval.definition')}</span>
              <select
                id={`${id}-definition`}
                name="definition"
                className="input"
                required
                defaultValue=""
                disabled={pending || catalog.loading || Boolean(catalog.error)}
              >
                <option value="">{t('swarm_eval.choose')}</option>
                {catalog.data?.map((definition) => (
                  <option key={definition.name} value={definition.name}>
                    {definition.name}
                  </option>
                ))}
              </select>
            </label>
            <LoadState
              loading={catalog.loading}
              error={catalog.error ?? ''}
              retry={catalog.refresh}
            >
              {null}
            </LoadState>
            {field('model')}
            <p>{t('swarm_eval.modelHint')}</p>
            <Button
              type="submit"
              disabled={pending || !catalog.data?.length || Boolean(catalog.error)}
            >
              {t('swarm_eval.bind')}
            </Button>
          </form>
        ) : null}
        {refusal && (
          <p className={styles.operationFailure} role="alert">
            {refusal}
          </p>
        )}
      </Section>
    </div>
  );
}
