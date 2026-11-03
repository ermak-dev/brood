import { useId, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { Select } from '@/components/ui/Select';
import { CommandError, sessionApi, type CommandResult } from '@/services/brood/commands';
import { errorMessage } from '@/services/brood/http';
import { ConfirmDialog } from './ConfirmDialog';
import { Field, Section } from './form';
import styles from '../substrate.module.scss';

/** Белый список slash-команд сервера (docs/MCP.md); другие он отвергает. */
const SLASH = ['/status', '/compact', '/clear', '/model', '/rc', 'Escape'] as const;
type Slash = (typeof SLASH)[number];
const slashKey = (command: Slash) => (command === 'Escape' ? 'escape' : command.slice(1));

const MAX_WAIT = 300;

type Outcome = { kind: 'ok' | 'unknown' | 'error'; text: string };

async function attempt(call: () => Promise<CommandResult>): Promise<Outcome> {
  try {
    const result = await call();
    return { kind: result.unconfirmed ? 'unknown' : 'ok', text: result.text };
  } catch (error) {
    return error instanceof CommandError && error.unconfirmed
      ? { kind: 'unknown', text: error.message }
      : { kind: 'error', text: errorMessage(error) };
  }
}

/** Итог отправки: неизвестный исход остаётся, пока его не закроют, —
 * повтор может доставить сообщение дважды. */
function OutcomeNotice({
  outcome,
  success,
  unknown,
  onDismiss,
}: {
  outcome: Outcome;
  success: string;
  unknown: string;
  onDismiss: () => void;
}) {
  const { t } = useTranslation();
  if (outcome.kind === 'ok') {
    return (
      <p role="status" className={styles.notice}>
        {success}
        {outcome.text && <span className={styles.mono}> {outcome.text}</span>}
      </p>
    );
  }
  return (
    <div
      role="alert"
      className={`${styles.notice} ${outcome.kind === 'unknown' ? styles.unknownNotice : styles.operationFailure}`}
    >
      {outcome.kind === 'unknown' && <strong>{unknown}</strong>}
      {outcome.text && <p className={styles.mono}>{outcome.text}</p>}
      <Button type="button" variant="ghost" size="sm" onClick={onDismiss}>
        {t('actors.ops.dismiss')}
      </Button>
    </div>
  );
}

/** brood: сообщение актору и slash-команда в его сессию. Сообщение запускает
 * ход модели и стоит денег — форма говорит об этом до отправки; ожидание
 * квитанции выключено по умолчанию. Ответ модели сюда не приходит. */
export function ActorMessage({ swarm, name }: { swarm: string; name: string }) {
  const { t } = useTranslation();
  const messageId = useId();
  const costId = useId();
  const [message, setMessage] = useState('');
  const [wait, setWait] = useState(false);
  const [seconds, setSeconds] = useState('30');
  const [sending, setSending] = useState(false);
  const [sent, setSent] = useState<Outcome | null>(null);
  const [command, setCommand] = useState<Slash>('/status');
  const [slashing, setSlashing] = useState(false);
  const [slashed, setSlashed] = useState<Outcome | null>(null);
  const [confirmClear, setConfirmClear] = useState(false);

  const waitSeconds = Number(seconds);
  const waitValid = !wait || (Number.isInteger(waitSeconds) && waitSeconds >= 1 && waitSeconds <= MAX_WAIT);
  const canSend = message.trim() !== '' && waitValid && !sending;

  const send = async () => {
    if (!canSend) return;
    setSending(true);
    setSent(null);
    const outcome = await attempt(() => sessionApi.send(swarm, name, message, wait ? waitSeconds : 0));
    setSent(outcome);
    if (outcome.kind === 'ok') setMessage('');
    setSending(false);
  };

  const slash = async () => {
    if (slashing) return;
    setSlashing(true);
    setSlashed(null);
    setSlashed(await attempt(() => sessionApi.slash(swarm, name, command)));
    setSlashing(false);
  };

  return (
    <div className={styles.form}>
      <Section title={t('actors.console.message.title')}>
        <form
          className={styles.section}
          onSubmit={(event) => {
            event.preventDefault();
            void send();
          }}
        >
          <p id={costId} className={`${styles.notice} ${styles.costNotice}`}>
            {t('actors.console.message.cost')}
          </p>
          <div className={styles.field}>
            <label className={styles.fieldLabel} htmlFor={messageId}>
              {t('actors.console.message.label')}
            </label>
            <textarea
              id={messageId}
              className={styles.messageArea}
              value={message}
              maxLength={262144}
              aria-describedby={costId}
              onChange={(event) => setMessage(event.target.value)}
            />
            <p className={styles.fieldHint}>{t('actors.console.message.hint')}</p>
          </div>
          <label className={styles.checkRow}>
            <input type="checkbox" checked={wait} onChange={(event) => setWait(event.target.checked)} />
            {t('actors.console.message.wait')}
          </label>
          {wait && (
            <Field
              label={t('actors.console.message.wait_seconds')}
              hint={t('actors.console.message.wait_hint')}
              error={waitValid ? undefined : t('actors.console.message.wait_invalid', { max: MAX_WAIT })}
            >
              {(id) => (
                <input
                  id={id}
                  className={`input ${styles.narrowInput}`}
                  type="number"
                  min={1}
                  max={MAX_WAIT}
                  step={1}
                  value={seconds}
                  onChange={(event) => setSeconds(event.target.value)}
                />
              )}
            </Field>
          )}
          <div className={styles.consoleControls}>
            <Button type="submit" loading={sending} disabled={!canSend}>
              {t('actors.console.message.send')}
            </Button>
          </div>
          {sent && (
            <OutcomeNotice
              outcome={sent}
              success={t('actors.console.message.sent')}
              unknown={t('actors.console.message.unknown')}
              onDismiss={() => setSent(null)}
            />
          )}
        </form>
      </Section>

      <Section title={t('actors.console.slash.title')} hint={t('actors.console.slash.hint')}>
        <form
          className={styles.section}
          onSubmit={(event) => {
            event.preventDefault();
            if (command === '/clear') setConfirmClear(true);
            else void slash();
          }}
        >
          <Field
            label={t('actors.console.slash.command')}
            hint={t(`actors.console.slash.commands.${slashKey(command)}`)}
          >
            {(id) => (
              <Select
                id={id}
                value={command}
                options={SLASH.map((value) => ({ value, label: value }))}
                onChange={(value) => setCommand(value as Slash)}
              />
            )}
          </Field>
          <div className={styles.consoleControls}>
            <Button type="submit" variant="secondary" loading={slashing}>
              {t('actors.console.slash.send')}
            </Button>
          </div>
          {slashed && (
            <OutcomeNotice
              outcome={slashed}
              success={t('actors.console.slash.sent')}
              unknown={t('actors.console.slash.unknown')}
              onDismiss={() => setSlashed(null)}
            />
          )}
        </form>
      </Section>

      <ConfirmDialog
        open={confirmClear}
        title={t('actors.console.slash.clear_title', { name })}
        confirmText={t('actors.console.slash.clear_confirm')}
        onCancel={() => setConfirmClear(false)}
        onConfirm={() => {
          setConfirmClear(false);
          void slash();
        }}
      >
        <p>{t('actors.console.slash.clear_message')}</p>
      </ConfirmDialog>
    </div>
  );
}
