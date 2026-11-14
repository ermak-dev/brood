import { useEffect, useId, useMemo, useState, type FormEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { Input } from '@/components/ui/Input';
import { ToggleSwitch } from '@/components/ui/ToggleSwitch';
import { useUnsavedChangesGuard } from '@/hooks/useUnsavedChangesGuard';
import { LoadState, StatusBadge } from '@/features/access/shared';
import { CopyButton } from '@/features/oauth/components/CopyButton';
import { ConfirmDialog } from '@/features/substrate/components/ConfirmDialog';
import { Section } from '@/features/substrate/components/form';
import { errorMessage } from '@/services/brood/http';
import type { TelegramChat, TelegramLinkCode, TelegramProfile } from '@/services/brood/telegram';
import {
  EMPTY_DRAFT,
  codeCountdown,
  linkBlocker,
  liveCode,
  removal,
  subscriptionRows,
  telegramTone,
  toggledEvents,
  tokenDraft,
  tokenProblem,
  tokenValue,
  waitsForStart,
  type Removal,
} from './telegramState';
import { useTelegramProfile } from './useTelegramProfile';
import styles from '@/features/substrate/substrate.module.scss';
import local from './profile.module.scss';

/** Отказ показывается у действия, которое его получило. */
type Action = 'token' | 'link' | 'remove' | 'events';

/** Имя поля токена в форме: значение берётся из него при отправке. */
const TOKEN_FIELD = 'telegram-bot-token';

const handle = (username: string) => `@${username}`;

/** Время срока кода в локали панели: код живёт 15 минут, дата лишняя. */
const shortTime = (moment: string, language: string) =>
  new Intl.DateTimeFormat(language, { timeStyle: 'short' }).format(new Date(moment));

/** Срок кода и сколько до него осталось, словами локали. */
function CodeExpiry({ expiresAt, now }: { expiresAt: string; now: number }) {
  const { t, i18n } = useTranslation();
  const countdown = codeCountdown(expiresAt, now);
  if (!countdown) return null;
  const left = countdown.expired
    ? t('profile.telegram.code_expired')
    : countdown.minutes === 0
      ? t('profile.telegram.under_minute')
      : t('profile.telegram.minutes_left', { count: countdown.minutes });
  return (
    <time dateTime={expiresAt} title={new Date(expiresAt).toLocaleString(i18n.language)}>
      {shortTime(expiresAt, i18n.language)} · {left}
    </time>
  );
}

/** Выданный на странице код: ссылка t.me с копированием, сам код и срок.
 * Без имени бота ссылки нет -- код отправляют боту командой /start. */
function IssuedCode({ code, now }: { code: TelegramLinkCode; now: number }) {
  const { t } = useTranslation();
  return (
    <div className={local.code}>
      {code.url ? (
        <>
          <p className={styles.fieldHint}>
            {t('profile.telegram.link_open_hint', { bot: handle(code.botUsername) })}
          </p>
          <div className={local.linkRow}>
            <a
              className="btn btn-primary btn-sm"
              href={code.url}
              target="_blank"
              rel="noopener noreferrer"
            >
              <span>{t('profile.telegram.link_open', { bot: handle(code.botUsername) })}</span>
            </a>
            <CopyButton value={code.url} label={t('profile.telegram.copy_link')} showLabel />
          </div>
          <p className={`${styles.mono} ${local.url}`}>{code.url}</p>
        </>
      ) : (
        <p className={styles.fieldHint}>{t('profile.telegram.link_manual', { code: code.code })}</p>
      )}
      <dl className={styles.details}>
        <dt>{t('profile.telegram.link_code')}</dt>
        <dd className={local.codeValue}>
          <span className={styles.mono}>{code.code}</span>
          <CopyButton value={code.code} label={t('profile.telegram.copy_code')} />
        </dd>
        <dt>{t('profile.telegram.link_expires')}</dt>
        <dd>
          <CodeExpiry expiresAt={code.expiresAt} now={now} />
        </dd>
      </dl>
      <p className={styles.notice} role="status">
        {t('profile.telegram.link_waiting')}
      </p>
    </div>
  );
}

interface LinkStepProps {
  profile: TelegramProfile;
  code: TelegramLinkCode | null;
  now: number;
  busy: boolean;
  issuing: boolean;
  refusal: string;
  onIssue: () => void;
}

/** Привязка чата: выдать код, показать его и ждать /start, или сказать,
 * почему кода нет. */
function LinkStep({ profile, code, now, busy, issuing, refusal, onIssue }: LinkStepProps) {
  const { t, i18n } = useTranslation();
  const blocker = linkBlocker(profile);
  if (blocker === 'linked') {
    return <p className={styles.sectionHint}>{t('profile.telegram.linked_hint')}</p>;
  }
  if (blocker) {
    return (
      <p className={styles.notice}>
        {t(
          blocker === 'no_bot'
            ? 'profile.telegram.blocked_no_bot'
            : 'profile.telegram.blocked_install_stopped'
        )}
      </p>
    );
  }
  // Ждёт ли код и какой показать, решает сервер; часы страницы (уже по
  // часам сервера) только отличают истёкший код от снятого.
  const live = liveCode(code, profile);
  const waiting = waitsForStart(profile);
  const expired = Boolean(code && !waiting && codeCountdown(code.expiresAt, now)?.expired);
  return (
    <>
      {live && <IssuedCode code={live} now={now} />}
      {!live && waiting && profile.linkCodeExpiresAt && (
        <p className={styles.notice}>
          {t('profile.telegram.link_pending_elsewhere', {
            time: shortTime(profile.linkCodeExpiresAt, i18n.language),
          })}
        </p>
      )}
      {expired && <p className={styles.notice}>{t('profile.telegram.link_expired')}</p>}
      <div className={local.actions}>
        <Button
          variant={waiting ? 'secondary' : 'primary'}
          size="sm"
          onClick={onIssue}
          disabled={busy}
          loading={issuing}
        >
          {t(waiting ? 'profile.telegram.link_reissue' : 'profile.telegram.link_issue')}
        </Button>
      </div>
      {refusal && (
        <p className={`${styles.notice} ${styles.operationFailure}`} role="alert">
          {t('profile.telegram.refused', { reason: refusal })}
        </p>
      )}
    </>
  );
}

/** Секция Telegram профиля (#285): бот привязки и состояние, токен своего
 * бота (write-only), привязка чата кодом, подписки и отвязка. */
export function TelegramSection() {
  const { t } = useTranslation();
  const [notice, setNotice] = useState('');
  const chatLabel = (chat: TelegramChat | null) =>
    chat?.username
      ? handle(chat.username)
      : t('profile.telegram.chat_user', { id: chat?.telegramUserId ?? '' });
  // Строка состояния говорит о последней смене: новая смена её заменяет,
  // иначе «Чат привязан» висел бы над ошибкой привязки.
  const model = useTelegramProfile((next) =>
    setNotice(
      next.state === 'linked'
        ? t('profile.telegram.linked_notice', { chat: chatLabel(next.chat) })
        : ''
    )
  );
  const { profile, error, code, now, busy } = model;
  const tokenId = useId();
  // Набранный токен есть только в значении поля: поле неуправляемое, и React
  // не копирует его ни в состояние, ни в атрибут value разметки (её читает
  // всё, что сериализует DOM). Состояние знает лишь, есть ли что сохранить и
  // какой формы набранное; после сохранения поле пустеет.
  const [draft, setDraft] = useState(EMPTY_DRAFT);
  const [tokenTouched, setTokenTouched] = useState(false);
  const [running, setRunning] = useState<Action | null>(null);
  const [refusal, setRefusal] = useState<{ action: Action; reason: string } | null>(null);
  const [confirmKind, setConfirmKind] = useState<Removal>('unlink');
  const [confirmOpen, setConfirmOpen] = useState(false);

  const unsavedDialog = useMemo(
    () => ({
      title: t('common.unsaved_changes_title'),
      message: t('profile.telegram.unsaved_message'),
      confirmText: t('common.leave'),
      cancelText: t('common.stay'),
      variant: 'danger' as const,
    }),
    [t]
  );
  useUnsavedChangesGuard({
    shouldBlock: draft.filled && running !== 'token',
    dialog: unsavedDialog,
  });

  // Переключатель в фокусе на время записи выключен, и браузер уводит фокус
  // на body: второй Space листал бы страницу. После ответа фокус
  // возвращается к нему, если пользователь не увёл его сам.
  const eventsId = useId();
  const switchId = (kind: string) => `${eventsId}-${kind}`;
  const [refocus, setRefocus] = useState('');
  useEffect(() => {
    if (busy || !refocus) return;
    setRefocus('');
    const target = document.getElementById(refocus);
    const active = document.activeElement;
    if (target && (!active || active === document.body)) target.focus();
  }, [busy, refocus]);

  /** Действие с одним исходом на странице: строка состояния или отказ у
   * этого действия. */
  const perform = async (action: Action, run: () => Promise<string>) => {
    setRunning(action);
    setRefusal(null);
    setNotice('');
    try {
      setNotice(await run());
    } catch (e) {
      setRefusal({ action, reason: errorMessage(e) });
    } finally {
      setRunning(null);
    }
  };

  const save = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const form = event.currentTarget;
    const field = form.elements.namedItem(TOKEN_FIELD);
    const typed = field instanceof window.HTMLInputElement ? field.value : '';
    if (busy || !tokenValue(typed)) return;
    if (tokenProblem(typed)) {
      setTokenTouched(true);
      return;
    }
    void perform('token', async () => {
      const next = await model.saveToken(tokenValue(typed));
      form.reset();
      setDraft(EMPTY_DRAFT);
      setTokenTouched(false);
      return t('profile.telegram.token_saved', { bot: handle(next.botUsername) });
    });
  };

  const issue = () =>
    void perform('link', async () => {
      await model.issueCode();
      return '';
    });

  /** Подписка уходит полным набором; профиль ответа ложится, как у
   * остальных записей секции. */
  const switchEvent = async (kind: string, on: boolean) => {
    if (busy || !profile) return;
    const events = toggledEvents(profile.events, kind, on);
    await perform('events', async () => {
      await model.saveEvents(events);
      return '';
    });
    setRefocus(switchId(kind));
  };

  const confirmRemoval = () => {
    setConfirmOpen(false);
    void perform('remove', async () => {
      await model.remove();
      return t(
        confirmKind === 'remove_bot' ? 'profile.telegram.removed' : 'profile.telegram.unlinked'
      );
    });
  };

  if (!profile) {
    return (
      <Section title={t('profile.telegram.title')} hint={t('profile.telegram.hint')}>
        <LoadState loading={!error} error={error} retry={model.refresh}>
          {null}
        </LoadState>
      </Section>
    );
  }

  const waiting = waitsForStart(profile);
  const kind = removal(profile);
  const bot = profile.botUsername ? handle(profile.botUsername) : '';
  const stateLabel = waiting
    ? t('profile.telegram.state.waiting')
    : t(`profile.telegram.state.${profile.state}`, profile.state);
  const tokenHint = profile.ownBot
    ? t('profile.telegram.token_hint_own', { bot })
    : bot
      ? t('profile.telegram.token_hint_install', { bot })
      : t('profile.telegram.token_hint_none');
  const failure = (action: Action) =>
    refusal?.action === action ? (
      <p className={`${styles.notice} ${styles.operationFailure}`} role="alert">
        {t('profile.telegram.refused', { reason: refusal.reason })}
      </p>
    ) : null;

  return (
    <Section
      title={t('profile.telegram.title')}
      hint={t('profile.telegram.hint')}
      action={<StatusBadge tone={telegramTone(profile)}>{stateLabel}</StatusBadge>}
    >
      {error && (
        <p className={`${styles.notice} ${styles.operationFailure}`} role="alert">
          {t('profile.telegram.refresh_failed', { reason: error })}
        </p>
      )}
      {notice && (
        <p className={styles.notice} role="status">
          {notice}
        </p>
      )}

      <div className={styles.card}>
        <div className={local.summary}>
          <dl className={styles.details}>
            <dt>{t('profile.telegram.bot')}</dt>
            <dd>
              {bot ? (
                <>
                  <span className={styles.mono}>{bot}</span>
                  <span className={styles.muted}>
                    {' · '}
                    {t(
                      profile.ownBot
                        ? 'profile.telegram.own_bot'
                        : 'profile.telegram.installation_bot'
                    )}
                  </span>
                </>
              ) : (
                t('profile.telegram.no_bot')
              )}
            </dd>
            <dt>{t('profile.telegram.chat')}</dt>
            <dd>
              {profile.state === 'linked' && profile.chat ? (
                <>
                  <span className={styles.mono}>{chatLabel(profile.chat)}</span>
                  {profile.chat.username && (
                    <span className={styles.muted}>
                      {' · '}
                      {t('profile.telegram.chat_user', { id: profile.chat.telegramUserId })}
                    </span>
                  )}
                </>
              ) : (
                t('profile.telegram.chat_none')
              )}
            </dd>
          </dl>
          {kind && (
            <Button
              variant="ghost"
              size="sm"
              className={styles.dangerText}
              disabled={busy}
              loading={running === 'remove'}
              onClick={() => {
                setConfirmKind(kind);
                setConfirmOpen(true);
              }}
            >
              {t(kind === 'remove_bot' ? 'profile.telegram.remove_bot' : 'profile.telegram.unlink')}
            </Button>
          )}
        </div>
        {profile.state === 'error' && profile.reason && (
          <p className={`${styles.notice} ${styles.operationFailure}`} role="alert">
            {t('profile.telegram.error_reason', { reason: profile.reason })}
          </p>
        )}
        {!profile.ownBot && profile.botReason && (
          <p className={`${styles.notice} ${styles.operationFailure}`} role="alert">
            {t('profile.telegram.install_stopped', { reason: profile.botReason })}
          </p>
        )}
        {failure('remove')}
      </div>

      <Section title={t('profile.telegram.link_title')} hint={t('profile.telegram.link_hint')}>
        <LinkStep
          profile={profile}
          code={code}
          now={now}
          busy={busy}
          issuing={running === 'link'}
          refusal={refusal?.action === 'link' ? refusal.reason : ''}
          onIssue={issue}
        />
      </Section>

      <Section title={t('profile.telegram.token_title')} hint={tokenHint}>
        <form className={local.tokenForm} onSubmit={save} noValidate>
          <Input
            id={tokenId}
            name={TOKEN_FIELD}
            type="password"
            label={t(
              profile.ownBot
                ? 'profile.telegram.token_label_replace'
                : 'profile.telegram.token_label'
            )}
            defaultValue=""
            autoComplete="new-password"
            autoCapitalize="off"
            spellCheck={false}
            placeholder="123456789:AA…"
            disabled={busy}
            hint={t('profile.telegram.token_write_only')}
            error={tokenTouched && draft.problem ? t('profile.telegram.token_shape') : undefined}
            onBlur={() => setTokenTouched(true)}
            onChange={(event) => setDraft(tokenDraft(event.target.value))}
          />
          <div className={local.actions}>
            <Button
              type="submit"
              size="sm"
              disabled={busy || !draft.filled}
              loading={running === 'token'}
            >
              {t('profile.telegram.token_save')}
            </Button>
          </div>
          {failure('token')}
        </form>
      </Section>

      <Section title={t('profile.telegram.events_title')} hint={t('profile.telegram.events_hint')}>
        <ul className={local.events}>
          {subscriptionRows(profile.events).map((row) => (
            <li key={row.kind}>
              {/* Переключатель показывает состояние сервера: набор меняет
                  ответ записи, а не щелчок. Вид сервера новее панель не
                  переключает: выключенный, он пропал бы из списка. */}
              <ToggleSwitch
                id={switchId(row.kind)}
                checked={row.on}
                disabled={busy || !row.known}
                onChange={(on) => void switchEvent(row.kind, on)}
                label={row.known ? t(`profile.telegram.events.${row.kind}`) : row.kind}
              />
            </li>
          ))}
        </ul>
        {failure('events')}
      </Section>

      <ConfirmDialog
        open={confirmOpen}
        title={t(
          confirmKind === 'remove_bot'
            ? 'profile.telegram.remove_bot_title'
            : 'profile.telegram.unlink_title'
        )}
        confirmText={t(
          confirmKind === 'remove_bot' ? 'profile.telegram.remove_bot' : 'profile.telegram.unlink'
        )}
        onConfirm={confirmRemoval}
        onCancel={() => setConfirmOpen(false)}
      >
        <p>
          {confirmKind === 'unlink'
            ? t('profile.telegram.unlink_message', { chat: chatLabel(profile.chat) })
            : profile.state === 'linked'
              ? t('profile.telegram.remove_bot_message', { bot })
              : t('profile.telegram.remove_bot_unlinked_message', { bot })}
        </p>
      </ConfirmDialog>
    </Section>
  );
}
