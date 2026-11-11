/**
 * Чистая логика запросов оператору на странице роя (#216): какую форму
 * показывает запрос, что уходит в ответ, порядок инбокса, что осталось
 * от ответа в истории и как повторять его доставку. Компоненты только
 * рисуют результат.
 */
import type { AccessSwarm } from '@/services/brood/access';
import type { RequestAnswer, SwarmRequest } from '@/services/brood/requests';

/** Черновик ответа: выбранное значение варианта и текст. */
export interface RequestDraft {
  choice: string | null;
  text: string;
}

export function emptyDraft(): RequestDraft {
  return { choice: null, text: '' };
}

export interface RequestChoice {
  /** Что уходит в ответ: исход узлового варианта или текст ответа актору. */
  value: string;
  label: string;
}

export interface RequestForm {
  choices: RequestChoice[];
  /** Выбор ведёт граф: так только у узлового запроса. */
  route: boolean;
  /** Варианты — да и нет; подписи переводит компонент. */
  confirm: boolean;
  text: 'none' | 'optional' | 'required';
}

const CONFIRM_VALUES = ['yes', 'no'];

/** Форма по виду запроса. Узловой: выбор исхода обязателен, текст — нет.
 * Запрос актора граф не ведёт: его выбор и подтверждение — текст ответа,
 * а исхода у него нет вовсе. Незнакомое происхождение формы не получает:
 * панель не угадывает, как отвечать. */
export function requestForm(request: SwarmRequest): RequestForm | null {
  if (request.origin === 'node') {
    return {
      choices: request.options.map((option) => ({ value: option.outcome, label: option.label })),
      route: true,
      confirm: false,
      text: 'optional',
    };
  }
  if (request.origin !== 'actor') return null;
  switch (request.kind) {
    case 'choice':
      return {
        choices: request.options.map((option) => ({ value: option.label, label: option.label })),
        route: false,
        confirm: false,
        text: 'none',
      };
    case 'confirm':
      return {
        choices: CONFIRM_VALUES.map((value) => ({ value, label: value })),
        route: false,
        confirm: true,
        text: 'none',
      };
    default:
      return { choices: [], route: false, confirm: false, text: 'required' };
  }
}

/** Тело ответа или null, пока ответ неполон. Выбор — только из вариантов
 * запроса; исход уходит только у узлового запроса. */
export function answerBody(request: SwarmRequest, draft: RequestDraft): RequestAnswer | null {
  const form = requestForm(request);
  if (!form) return null;
  const text = draft.text.trim();
  if (form.text === 'required' && text === '') return null;
  if (form.choices.length === 0 && form.text !== 'none') {
    return form.text === 'required' ? { response: text } : null;
  }
  const choice = form.choices.find((option) => option.value === draft.choice);
  if (!choice) return null;
  if (!form.route) return { response: choice.value };
  return text === '' ? { outcome: choice.value } : { outcome: choice.value, response: text };
}

const byTime = (left: string, right: string) => (left < right ? -1 : left > right ? 1 : 0);

const byAsked = (left: SwarmRequest, right: SwarmRequest) =>
  byTime(left.createdAt, right.createdAt);

/** Инбокс: открытые сверху, дольше ждущий первым; ниже история, свежий
 * ответ первым. Запрос, на который отвечали на этой странице (handledHere:
 * ответ принят или отклонён), остаётся на своём месте среди открытых
 * (#259): подтверждение или отказ не уходят в свёрнутую историю, а open
 * считает только ждущие ответа.
 *
 * Ответ, которого актор не получил (`failed`) или получил без квитанции
 * (`unconfirmed`), не история (#264): оператор должен его видеть, пока
 * доставка не удалась. Такие запросы стоят своей группой под открытыми,
 * дольше ждущий актор первым. Повтор доставки на этой странице
 * (retriedHere) оставляет карточку в группе и после удачи, до ухода со
 * страницы: новое состояние и есть итог повтора, на нём фокус. Счёт
 * недоставленных (undelivered) берёт каждый такой ответ, где бы ни стояла
 * его карточка, в том числе отвеченный здесь. */
export function inboxSections(
  requests: SwarmRequest[] | null,
  handledHere: readonly string[] = [],
  retriedHere: readonly string[] = []
): {
  open: SwarmRequest[];
  /** Карточки над историей: открытые и те, на которые отвечали здесь, по
   * моменту вопроса. */
  current: SwarmRequest[];
  /** Группа «Доставка требует внимания»: недоставленные ответы не из
   * current и повторённые здесь, по моменту вопроса. */
  attention: SwarmRequest[];
  /** Все недоставленные ответы, по моменту вопроса: их число стоит в шапке. */
  undelivered: SwarmRequest[];
  history: SwarmRequest[];
} {
  const all = requests ?? [];
  const answeredHere = new Set(handledHere);
  const retried = new Set(retriedHere);
  const stays = (request: SwarmRequest) => request.state === 'open' || answeredHere.has(request.id);
  const waitsDelivery = (request: SwarmRequest) =>
    !stays(request) && (deliveryRetryable(request) || retried.has(request.id));
  const current = all.filter(stays).sort(byAsked);
  const open = current.filter((request) => request.state === 'open');
  const attention = all.filter(waitsDelivery).sort(byAsked);
  const undelivered = all.filter(deliveryRetryable).sort(byAsked);
  const history = all
    .filter((request) => !stays(request) && !waitsDelivery(request))
    .sort((left, right) =>
      byTime(right.answeredAt ?? right.createdAt, left.answeredAt ?? left.createdAt)
    );
  return { open, current, attention, undelivered, history };
}

/** Что осталось от ответа: подпись нажатого варианта, а не машинный
 * исход; да/нет подтверждения; свободный текст. */
export interface AnswerSummary {
  choice?: string;
  confirm?: string;
  text?: string;
}

export function answerSummary(request: SwarmRequest): AnswerSummary | null {
  if (request.state !== 'answered') return null;
  const summary: AnswerSummary = {};
  if (request.origin === 'node') {
    const listed = request.options.find((option) => option.outcome === request.outcome)?.label;
    const choice = request.optionLabel || listed || request.outcome;
    if (choice) summary.choice = choice;
    if (request.response) summary.text = request.response;
    return summary;
  }
  if (!request.response) return summary;
  if (request.kind === 'confirm' && CONFIRM_VALUES.includes(request.response)) {
    summary.confirm = request.response;
  } else if (request.kind === 'choice') {
    summary.choice = request.response;
  } else {
    summary.text = request.response;
  }
  return summary;
}

/** Запрос выбранного узла: открытый, иначе последнего посещения. */
export function nodeRequest(
  requests: SwarmRequest[] | null,
  nodeId: string | undefined
): SwarmRequest | undefined {
  if (nodeId === undefined) return undefined;
  const asked = (requests ?? []).filter(
    (request) => request.origin === 'node' && request.nodeId === nodeId
  );
  const open = asked.find((request) => request.state === 'open');
  if (open) return open;
  return asked.reduce<SwarmRequest | undefined>(
    (latest, request) =>
      !latest || (request.sequence ?? 0) > (latest.sequence ?? 0) ? request : latest,
    undefined
  );
}

/** Отвечает управляющий живым роем (owner, editor; администратор — owner);
 * окончательную проверку делает сервер в транзакции ответа. */
export function canAnswerRequests(swarm: AccessSwarm | null): boolean {
  return Boolean(
    swarm && swarm.state === 'active' && (swarm.role === 'owner' || swarm.role === 'editor')
  );
}

/** Повтор доставки — у ответа актору, который заведомо не ушёл (`failed`)
 * или ушёл без квитанции (`unconfirmed`, #260). Идущую и доставленную
 * доставку не повторяют. Тот же ответ инбокс держит на виду и считает
 * недоставленным (#264): то, что можно повторить, ждёт решения оператора. */
export function deliveryRetryable(request: SwarmRequest): boolean {
  return (
    request.origin === 'actor' &&
    request.state === 'answered' &&
    (request.delivery === 'failed' || request.delivery === 'unconfirmed')
  );
}

/** Повтор может доставить ответ дважды (#261): неподтверждённый ответ мог
 * уже дойти до актора, поэтому перед повтором панель спрашивает. Повтор
 * `failed` безопасен и идёт сразу. */
export function retryMayDuplicate(request: SwarmRequest): boolean {
  return deliveryRetryable(request) && request.delivery === 'unconfirmed';
}
