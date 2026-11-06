import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import type { HumanGate } from '@/services/brood/pipelines';
import {
  gateDecision,
  gateKindAllowsOptions,
  gateReady,
  initialGateState,
  selectOption,
} from './state';
import styles from './pipelines.module.scss';

/** Вопрос живого прохода (#169): варианты с ускорителями, свободный текст
 * для freeform, подтверждение перед отправкой. Пока ответ уходит, всё
 * заблокировано, чтобы второй клик не отправил его дважды. */
export function HumanGatePanel({
  gate,
  submit,
}: {
  gate: HumanGate;
  submit: (answer: string) => Promise<void>;
}) {
  const { t, i18n } = useTranslation();
  const [state, setState] = useState(initialGateState);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState('');

  const send = async () => {
    if (submitting) return;
    const decision = gateDecision(gate, state);
    if (decision.act === 'confirm') {
      setState({ ...state, confirming: true });
      return;
    }
    if (decision.act !== 'send') return;
    setSubmitting(true);
    setError('');
    try {
      await submit(decision.answer);
    } catch (failure) {
      setError(failure instanceof Error ? failure.message : String(failure));
    } finally {
      setSubmitting(false);
      setState((current) => ({ ...current, confirming: false }));
    }
  };

  const ready = gateReady(gate, state);
  const deadline = gate.deadline ? new Date(gate.deadline).toLocaleString(i18n.language) : '';

  return (
    <form
      className={styles.form}
      onSubmit={(event) => {
        event.preventDefault();
        void send();
      }}
    >
      <p className={styles.gateQuestion}>{gate.question}</p>
      {deadline && (
        <p className={styles.gateDeadline}>{t('pipelines.gate_deadline', { deadline })}</p>
      )}
      {gateKindAllowsOptions(gate.kind) ? (
        <>
          <div className={styles.gateOptions} role="group" aria-label={gate.question}>
            {gate.options.map((option) => (
              <Button
                key={option.id}
                type="button"
                variant={state.choice === option.id ? 'primary' : 'secondary'}
                disabled={submitting}
                onClick={() => setState(selectOption(state, option.id))}
              >
                {option.label}
                {option.accelerator && (
                  <span className={styles.gateAccelerator}>{option.accelerator}</span>
                )}
              </Button>
            ))}
          </div>
          <Button type="submit" disabled={submitting || !ready}>
            {state.confirming ? t('pipelines.gate_confirm') : t('pipelines.gate_submit')}
          </Button>
          {gate.options.length === 0 && (
            <p className={styles.gateHint}>{t('pipelines.gate_no_options')}</p>
          )}
        </>
      ) : (
        <>
          <textarea
            className={styles.gateFreeform}
            value={state.freeform}
            placeholder={t('pipelines.gate_freeform_hint')}
            disabled={submitting}
            onChange={(event) => setState({ ...state, freeform: event.target.value })}
          />
          <Button type="submit" disabled={submitting || !ready}>
            {t('pipelines.gate_submit')}
          </Button>
        </>
      )}
      {state.confirming && !submitting && (
        <p className={styles.gateHint}>{t('pipelines.gate_confirm_hint')}</p>
      )}
      {error && (
        <p className={styles.gateError} role="alert">
          {error}
        </p>
      )}
    </form>
  );
}
