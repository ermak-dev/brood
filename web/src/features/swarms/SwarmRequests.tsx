import { useEffect, useId, useRef, useState, type FormEvent, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { Markdown } from '@/components/ui/Markdown';
import { prefersReducedMotion } from '@/hooks/motion';
import type { SwarmRequest } from '@/services/brood/requests';
import { deliveryTone, requestTone } from '@/features/access/state';
import { LoadState, StatusBadge } from '@/features/access/shared';
import { ConfirmDialog } from '@/features/substrate/components/ConfirmDialog';
import { Section } from '@/features/substrate/components/form';
import { DASH } from '@/features/substrate/format';
import { formatDateTimeValue } from '@/utils/format';
import {
  answerBody,
  answerSummary,
  deliveryRetryable,
  emptyDraft,
  inboxSections,
  requestForm,
  retryMayDuplicate,
} from './requestState';
import type { RequestInboxModel, RequestPlace } from './useSwarmRequests';
import styles from '@/features/substrate/substrate.module.scss';
import page from './SwarmPage.module.scss';
import local from './SwarmRequests.module.scss';

/** Инбокс роя: открытые запросы сверху и целиком, ниже отвеченные и
 * закрытые — историей, свёрнутой в счётчик (#253). Единственное место
 * ответа (#259): запрос, на который отвечали здесь, остаётся на своём
 * месте с подтверждением или отказом до ухода со страницы. Ответ, которого
 * актор не получил, в историю не уходит (#264): он стоит группой «Доставка
 * требует внимания» под открытыми, с повтором под рукой, а шапка считает
 * его рядом с открытыми. Узел запроса — кнопкой, она выбирает его на
 * графе. Пояснения у секции нет: карточка сама говорит, кто спрашивает, а
 * место на первом экране нужно вопросу и графу. */
export function RequestInbox({
  model,
  onSelectNode,
}: {
  model: RequestInboxModel;
  onSelectNode?: (node: string) => void;
}) {
  const { t } = useTranslation();
  const { open, current, attention, undelivered, history } = inboxSections(
    model.requests,
    model.handledHere,
    model.retriedHere
  );
  const card = (request: SwarmRequest) => (
    <li key={request.id}>
      <RequestCard model={model} request={request} onSelectNode={onSelectNode} />
    </li>
  );
  return (
    <Section
      title={t('swarm_requests.title')}
      action={
        open.length > 0 || undelivered.length > 0 ? (
          <span className={local.counts}>
            {open.length > 0 && (
              <StatusBadge tone="attention">
                {t('swarm_requests.open_count', { count: open.length })}
              </StatusBadge>
            )}
            {undelivered.length > 0 && (
              <StatusBadge tone="attention">
                {t('swarm_requests.undelivered_count', { count: undelivered.length })}
              </StatusBadge>
            )}
          </span>
        ) : undefined
      }
    >
      <LoadState
        loading={model.requests === null && !model.error}
        error={model.requests === null ? model.error : ''}
        retry={model.refresh}
      >
        {model.requests && model.error && (
          <p className={`${styles.notice} ${styles.operationFailure}`} role="alert">
            {model.error}
          </p>
        )}
        {open.length === 0 && <p className={page.hint}>{t('swarm_requests.none_open')}</p>}
        {current.length > 0 && <ol className={local.list}>{current.map(card)}</ol>}
        {attention.length > 0 && (
          <Section
            title={t('swarm_requests.attention_title')}
            hint={t('swarm_requests.attention_hint')}
          >
            <ol className={local.list}>{attention.map(card)}</ol>
          </Section>
        )}
        {history.length > 0 && (
          <details className={local.history}>
            <summary className={local.historySummary}>
              {t('swarm_requests.history_count', { count: history.length })}
            </summary>
            <ol className={local.list}>{history.map(card)}</ol>
          </details>
        )}
      </LoadState>
    </Section>
  );
}

/** Один запрос: кто и когда спросил, вопрос и материал, затем форма
 * ответа или то, чем он закрыт. Тот же компонент стоит у выбранного узла
 * графа (variant="node"): там вопрос уже показан узлом и не повторяется, а
 * формы нет (#259) — вместо неё кнопка к форме в инбоксе; подтверждение и
 * отказ тоже остаются инбоксу, узел показывает подпись выбранного варианта.
 * Материал свёрнут под своей подписью (#253): спрашивает вопрос, материал
 * читают по желанию. В широком инбоксе открытый запрос встаёт в две
 * колонки — вопрос слева, ответ справа, — чтобы граф остался на первом
 * экране. */
export function RequestCard({
  model,
  request,
  variant = 'inbox',
  onSelectNode,
}: {
  model: RequestInboxModel;
  request: SwarmRequest;
  variant?: RequestPlace;
  onSelectNode?: (node: string) => void;
}) {
  const { t, i18n } = useTranslation();
  const questionId = useId();
  const statusRef = useRef<HTMLParagraphElement>(null);
  const refusalRef = useRef<HTMLParagraphElement>(null);
  const replyRef = useRef<HTMLDivElement>(null);
  const cardRef = useRef<HTMLElement>(null);
  const atNode = variant === 'node';
  // Подтверждение и отказ — у формы, то есть в инбоксе.
  const notice = atNode ? undefined : model.notices[request.id];
  // Фокус берёт только карточка инбокса и только один раз: после отправки
  // форма может исчезнуть вместе с кнопкой в фокусе — фокус к итогу,
  // подтверждению или отказу; от узла («Ответить выше») — к выбору варианта.
  const target = !atNode && model.focus?.id === request.id ? model.focus.target : undefined;
  const takeFocus =
    target === 'result'
      ? Boolean(notice?.answered || notice?.refusal)
      : target === 'form' && request.state === 'open';
  const { focused } = model;
  useEffect(() => {
    if (!takeFocus) return;
    if (target === 'result') {
      (statusRef.current ?? refusalRef.current)?.focus();
    } else {
      const control = firstControl(replyRef.current);
      if (control) {
        control.focus({ preventScroll: true });
        cardRef.current?.scrollIntoView?.({
          behavior: prefersReducedMotion() ? 'auto' : 'smooth',
          block: 'center',
        });
      }
    }
    focused();
  }, [takeFocus, target, focused]);

  return (
    <article
      ref={cardRef}
      className={atNode ? `${local.card} ${local.embedded}` : local.card}
      data-state={request.state}
      data-undelivered={deliveryRetryable(request) ? request.delivery : undefined}
      aria-labelledby={atNode ? undefined : questionId}
      aria-label={atNode ? request.question : undefined}
    >
      <div className={local.meta}>
        <StatusBadge tone={requestTone(request.state)}>
          {t(`swarm_requests.states.${request.state}`, { defaultValue: request.state })}
        </StatusBadge>
        {!atNode && <Asker request={request} onSelectNode={onSelectNode} />}
        <span>
          {t('swarm_requests.asked')}{' '}
          <time dateTime={request.createdAt}>
            {formatDateTimeValue(request.createdAt, i18n.language) || request.createdAt}
          </time>
        </span>
      </div>
      {(!atNode || request.context) && (
        <div className={local.ask}>
          {!atNode && (
            <Markdown id={questionId} className={local.question} text={request.question} />
          )}
          {request.context && (
            <details className={local.context}>
              <summary className={local.contextSummary}>{t('swarm_requests.context')}</summary>
              <Markdown className={local.text} text={request.context} collapsible />
            </details>
          )}
        </div>
      )}
      {request.state === 'open' && (
        <div ref={replyRef} className={local.reply}>
          {!model.canAnswer ? (
            <p className={page.hint}>{t('swarm_requests.read_only')}</p>
          ) : atNode ? (
            <button type="button" className={local.above} onClick={() => model.reach(request)}>
              {t('swarm_requests.answer_above')}
            </button>
          ) : (
            <AnswerForm request={request} labelledBy={questionId} answer={model.answer} />
          )}
        </div>
      )}
      {request.state === 'answered' && (
        <AnswerDetails
          request={request}
          deliveryError={notice?.deliveryError}
          retry={model.canAnswer && deliveryRetryable(request) ? model.deliver : undefined}
        />
      )}
      {request.state === 'cancelled' && (
        <p className={page.hint}>{t('swarm_requests.cancelled_hint')}</p>
      )}
      {notice?.answered && (
        <p ref={statusRef} tabIndex={-1} role="status" className={local.sent}>
          {t('swarm_requests.sent')}
        </p>
      )}
      {notice?.refusal && (
        <p ref={refusalRef} tabIndex={-1} role="alert" className={local.refusal}>
          {t('swarm_requests.refused', { reason: notice.refusal })}
        </p>
      )}
    </article>
  );
}

/** Куда перевести фокус в форме ответа: на выбранный вариант, иначе на
 * первое поле — как при переходе в форму клавишей Tab. */
function firstControl(reply: HTMLElement | null): HTMLElement | null {
  return (
    reply?.querySelector<HTMLElement>('input:checked:not(:disabled)') ??
    reply?.querySelector<HTMLElement>('input:not(:disabled), textarea:not(:disabled)') ??
    null
  );
}

/** Кто спрашивает: узел графа (кнопкой — выбрать его) или актор по имени. */
function Asker({
  request,
  onSelectNode,
}: {
  request: SwarmRequest;
  onSelectNode?: (node: string) => void;
}) {
  const { t } = useTranslation();
  if (request.origin === 'node') {
    const node = request.nodeId;
    return (
      <span>
        {t('swarm_requests.asker_node')}:{' '}
        {node && onSelectNode ? (
          <button type="button" className={page.nodeButton} onClick={() => onSelectNode(node)}>
            {node}
          </button>
        ) : (
          <strong>{node || DASH}</strong>
        )}
      </span>
    );
  }
  return (
    <span>
      {t('swarm_requests.asker_actor')}:{' '}
      <strong>{request.actor?.name || t('swarm_requests.actor_gone')}</strong>
    </span>
  );
}

/** Форма по виду запроса. Выбор — радиогруппа: выбранный вариант отмечен
 * состоянием поля, а не только цветом. Неполный ответ не отправляется:
 * кнопка заблокирована, и отправка формы без тела ничего не шлёт. */
function AnswerForm({
  request,
  labelledBy,
  answer,
}: {
  request: SwarmRequest;
  labelledBy: string;
  answer: RequestInboxModel['answer'];
}) {
  const { t } = useTranslation();
  const [draft, setDraft] = useState(emptyDraft);
  const [sending, setSending] = useState(false);
  const group = useId();
  const hintId = useId();
  const form = requestForm(request);
  if (!form) return <p className={page.hint}>{t('swarm_requests.unknown_kind')}</p>;
  const body = answerBody(request, draft);

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (sending || !body) return;
    setSending(true);
    try {
      await answer(request, body);
    } finally {
      setSending(false);
    }
  };

  return (
    <form className={local.form} onSubmit={(event) => void submit(event)}>
      {form.choices.length > 0 && (
        <div
          role="radiogroup"
          aria-labelledby={labelledBy}
          aria-required="true"
          className={local.choices}
        >
          {form.choices.map((choice) => {
            const checked = draft.choice === choice.value;
            return (
              <label
                key={choice.value}
                className={local.choice}
                data-checked={checked || undefined}
              >
                <input
                  type="radio"
                  name={group}
                  value={choice.value}
                  checked={checked}
                  disabled={sending}
                  onChange={() => setDraft((current) => ({ ...current, choice: choice.value }))}
                />
                <span>
                  {form.confirm ? t(`swarm_requests.confirm.${choice.value}`) : choice.label}
                </span>
              </label>
            );
          })}
        </div>
      )}
      <p id={hintId} className={page.hint}>
        {t(form.route ? 'swarm_requests.route_hint' : 'swarm_requests.actor_hint')}
      </p>
      {form.text !== 'none' && (
        <label className={local.field}>
          <span className={local.label}>
            {t(form.text === 'required' ? 'swarm_requests.response' : 'swarm_requests.comment')}
          </span>
          <textarea
            className={local.textarea}
            value={draft.text}
            required={form.text === 'required'}
            aria-describedby={hintId}
            disabled={sending}
            onChange={(event) => {
              const text = event.target.value;
              setDraft((current) => ({ ...current, text }));
            }}
          />
        </label>
      )}
      <Button type="submit" disabled={!body} loading={sending}>
        {t('swarm_requests.submit')}
      </Button>
    </form>
  );
}

/** Чем закрыт запрос: подпись нажатого варианта, текст, автор и момент
 * ответа; у запроса актора — доставка ответа и её повтор при сбое. */
function AnswerDetails({
  request,
  deliveryError,
  retry,
}: {
  request: SwarmRequest;
  deliveryError?: string;
  retry?: (request: SwarmRequest) => Promise<void>;
}) {
  const { t, i18n } = useTranslation();
  const summary = answerSummary(request) ?? {};
  const rows: Array<{ label: string; value: ReactNode; text?: boolean }> = [];
  if (summary.choice) rows.push({ label: t('swarm_requests.answer'), value: summary.choice });
  if (summary.confirm) {
    rows.push({
      label: t('swarm_requests.answer'),
      value: t(`swarm_requests.confirm.${summary.confirm}`),
    });
  }
  if (summary.text) {
    rows.push({
      label: t(
        request.origin === 'node' ? 'swarm_requests.comment_saved' : 'swarm_requests.answer'
      ),
      value: <Markdown text={summary.text} />,
      text: true,
    });
  }
  rows.push({ label: t('swarm_requests.answered_by'), value: request.answeredBy || DASH });
  if (request.answeredAt) {
    rows.push({
      label: t('swarm_requests.answered_at'),
      value: (
        <time dateTime={request.answeredAt}>
          {formatDateTimeValue(request.answeredAt, i18n.language) || request.answeredAt}
        </time>
      ),
    });
  }
  return (
    <dl className={local.answer}>
      {rows.map((row) => (
        <div key={row.label}>
          <dt>{row.label}</dt>
          <dd className={row.text ? local.text : local.chosen}>{row.value}</dd>
        </div>
      ))}
      {request.delivery && (
        <div>
          <dt>{t('swarm_requests.delivery')}</dt>
          <Delivery request={request} reason={deliveryError} retry={retry} />
        </div>
      )}
    </dl>
  );
}

/** Доставка ответа актору (#260): состояние, причина сервера из ответа на
 * действие и повтор. Неподтверждённая (#261) говорит словами, что ответ мог
 * дойти, и спрашивает перед повтором: он может доставить ответ дважды;
 * повтор `failed` безопасен и идёт сразу. На время повтора кнопка
 * заблокирована, а после доставки исчезает, поэтому фокус переходит к
 * самой доставке: её новое состояние и причина — итог повтора. */
function Delivery({
  request,
  reason,
  retry,
}: {
  request: SwarmRequest;
  reason?: string;
  retry?: (request: SwarmRequest) => Promise<void>;
}) {
  const { t } = useTranslation();
  const [retrying, setRetrying] = useState(false);
  const [asking, setAsking] = useState(false);
  const outcomeRef = useRef<HTMLElement>(null);
  const delivery = request.delivery ?? '';
  const unconfirmed = delivery === 'unconfirmed';
  const twice = Boolean(retry) && retryMayDuplicate(request);
  // Причина прошлой неудачи не относится к доставленному ответу: карточка
  // остаётся на виду и после доставки, замеченной опросом (#264).
  const why = delivery === 'delivered' ? undefined : reason;

  const send = async () => {
    if (!retry || retrying) return;
    outcomeRef.current?.focus();
    setRetrying(true);
    try {
      await retry(request);
    } finally {
      setRetrying(false);
    }
  };
  const ask = () => {
    if (twice) setAsking(true);
    else void send();
  };

  return (
    <dd ref={outcomeRef} tabIndex={-1} className={local.delivery} aria-live="polite">
      <StatusBadge tone={deliveryTone(delivery)}>
        {t(`swarm_requests.delivery_states.${delivery}`, { defaultValue: delivery })}
      </StatusBadge>
      {unconfirmed ? (
        <span className={local.unconfirmed}>
          <span>{t('swarm_requests.unconfirmed_note')}</span>
          {why && <span className={local.reason}>{why}</span>}
        </span>
      ) : (
        why && <span>{why}</span>
      )}
      {retry && (
        <Button
          variant="secondary"
          size="sm"
          loading={retrying}
          aria-haspopup={twice ? 'dialog' : undefined}
          onClick={ask}
        >
          {t('swarm_requests.retry_delivery')}
        </Button>
      )}
      {twice && (
        <ConfirmDialog
          open={asking}
          title={t('swarm_requests.retry_title')}
          confirmText={t('swarm_requests.retry_delivery')}
          onCancel={() => setAsking(false)}
          onConfirm={() => {
            setAsking(false);
            void send();
          }}
        >
          <p>{t('swarm_requests.retry_twice')}</p>
        </ConfirmDialog>
      )}
    </dd>
  );
}
