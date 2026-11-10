import { useEffect, useId, useRef, useState, type FormEvent, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { Markdown } from '@/components/ui/Markdown';
import type { SwarmRequest } from '@/services/brood/requests';
import type { BadgeTone } from '@/features/access/state';
import { LoadState, StatusBadge } from '@/features/access/shared';
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
} from './requestState';
import type { RequestInboxModel, RequestPlace } from './useSwarmRequests';
import styles from '@/features/substrate/substrate.module.scss';
import page from './SwarmPage.module.scss';
import local from './SwarmRequests.module.scss';

function requestTone(state: string): BadgeTone {
  if (state === 'open') return 'warning';
  if (state === 'answered') return 'success';
  return 'muted';
}

function deliveryTone(delivery: string): BadgeTone {
  if (delivery === 'delivered') return 'success';
  if (delivery === 'failed') return 'danger';
  return 'warning';
}

/** Инбокс роя: открытые запросы сверху, ниже отвеченные и закрытые —
 * историей. Узел запроса — кнопкой, она выбирает его на графе. */
export function RequestInbox({
  model,
  onSelectNode,
}: {
  model: RequestInboxModel;
  onSelectNode?: (node: string) => void;
}) {
  const { t } = useTranslation();
  const { open, history } = inboxSections(model.requests);
  const card = (request: SwarmRequest) => (
    <li key={request.id}>
      <RequestCard model={model} request={request} onSelectNode={onSelectNode} />
    </li>
  );
  return (
    <Section
      title={t('swarm_requests.title')}
      hint={t('swarm_requests.hint')}
      action={
        open.length > 0 ? (
          <StatusBadge tone="warning">
            {t('swarm_requests.open_count', { count: open.length })}
          </StatusBadge>
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
        {open.length === 0 ? (
          <p className={page.hint}>{t('swarm_requests.none_open')}</p>
        ) : (
          <ol className={local.list}>{open.map(card)}</ol>
        )}
        {history.length > 0 && (
          <>
            <h4 className={local.historyTitle}>{t('swarm_requests.history')}</h4>
            <ol className={local.list}>{history.map(card)}</ol>
          </>
        )}
      </LoadState>
    </Section>
  );
}

/** Один запрос: кто и когда спросил, вопрос и материал, затем форма
 * ответа или то, чем он закрыт. Тот же компонент стоит у выбранного узла
 * графа (variant="node"): там вопрос уже показан узлом и не повторяется. */
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
  const notice = model.notices[request.id];
  const atNode = variant === 'node';
  // Отправленная форма исчезает вместе с кнопкой в фокусе: фокус переходит
  // к подтверждению там, откуда ответ ушёл, и только один раз.
  const takeFocus =
    Boolean(notice?.answered) && model.focus?.id === request.id && model.focus.place === variant;
  const { focused } = model;
  useEffect(() => {
    if (!takeFocus || !statusRef.current) return;
    statusRef.current.focus();
    focused();
  }, [takeFocus, focused]);

  return (
    <article
      className={atNode ? `${local.card} ${local.embedded}` : local.card}
      data-state={request.state}
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
      {!atNode && <Markdown id={questionId} className={local.question} text={request.question} />}
      {request.context && (
        <div className={local.context}>
          <p className={local.label}>{t('swarm_requests.context')}</p>
          <Markdown className={local.text} text={request.context} collapsible />
        </div>
      )}
      {request.state === 'open' &&
        (model.canAnswer ? (
          <AnswerForm
            request={request}
            place={variant}
            labelledBy={atNode ? undefined : questionId}
            answer={model.answer}
          />
        ) : (
          <p className={page.hint}>{t('swarm_requests.read_only')}</p>
        ))}
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
        <p role="alert" className={local.refusal}>
          {t('swarm_requests.refused', { reason: notice.refusal })}
        </p>
      )}
    </article>
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
  place,
  labelledBy,
  answer,
}: {
  request: SwarmRequest;
  place: RequestPlace;
  labelledBy?: string;
  answer: RequestInboxModel['answer'];
}) {
  const { t } = useTranslation();
  const [draft, setDraft] = useState(emptyDraft);
  const [sending, setSending] = useState(false);
  const group = useId();
  const form = requestForm(request);
  if (!form) return <p className={page.hint}>{t('swarm_requests.unknown_kind')}</p>;
  const body = answerBody(request, draft);

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (sending || !body) return;
    setSending(true);
    try {
      await answer(request, body, place);
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
          aria-label={labelledBy ? undefined : request.question}
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
      <p className={page.hint}>
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
  const [retrying, setRetrying] = useState(false);
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
  const delivery = request.delivery;
  return (
    <dl className={local.answer}>
      {rows.map((row) => (
        <div key={row.label}>
          <dt>{row.label}</dt>
          <dd className={row.text ? local.text : local.chosen}>{row.value}</dd>
        </div>
      ))}
      {delivery && (
        <div>
          <dt>{t('swarm_requests.delivery')}</dt>
          <dd className={local.delivery} aria-live="polite">
            <StatusBadge tone={deliveryTone(delivery)}>
              {t(`swarm_requests.delivery_states.${delivery}`, { defaultValue: delivery })}
            </StatusBadge>
            {deliveryError && <span>{deliveryError}</span>}
            {retry && (
              <Button
                variant="secondary"
                size="sm"
                loading={retrying}
                onClick={async () => {
                  setRetrying(true);
                  try {
                    await retry(request);
                  } finally {
                    setRetrying(false);
                  }
                }}
              >
                {t('swarm_requests.retry_delivery')}
              </Button>
            )}
          </dd>
        </div>
      )}
    </dl>
  );
}
