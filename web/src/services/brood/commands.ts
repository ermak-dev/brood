/**
 * brood: команды каталога bro из панели — POST /api/commands/{tool} с
 * {"swarm_id", "arguments"}. Тот же executor, что у CLI и MCP; сервер
 * проверяет права и предусловия при каждом вызове.
 *
 * Исход бывает неизвестным: RPC мог выполниться, а подтверждение потеряться.
 * Сервер помечает это заголовком X-Brood-Command-Outcome: unconfirmed (при
 * отказе) или outcome в теле. Такой исход не выдаётся ни за успех, ни за
 * обычный отказ: повтор без проверки может повторить эффект.
 */

import { BroodApiError, requestWithHeaders } from './http';

export interface CommandResult {
  text: string;
  data: unknown;
  outcome: string;
  /** Эффект мог произойти, а мог нет; повторять только после проверки. */
  unconfirmed: boolean;
}

export class CommandError extends BroodApiError {
  readonly unconfirmed: boolean;

  constructor(cause: BroodApiError, unconfirmed: boolean) {
    super(cause.status, cause.message, cause.body, cause.headers);
    this.name = 'CommandError';
    this.unconfirmed = unconfirmed;
  }
}

const OUTCOME_HEADER = 'X-Brood-Command-Outcome';
const UNKNOWN_OUTCOMES = new Set(['unconfirmed', 'unknown']);

const unknownHeader = (headers: Headers | undefined) =>
  headers?.get?.(OUTCOME_HEADER)?.toLowerCase() === 'unconfirmed';

export const commandsApi = {
  run: async (
    tool: string,
    swarm: string,
    args: Record<string, unknown>
  ): Promise<CommandResult> => {
    try {
      const { data, headers } = await requestWithHeaders<{
        text?: unknown;
        data?: unknown;
        outcome?: unknown;
      } | null>('POST', `/api/commands/${encodeURIComponent(tool)}`, {
        swarm_id: swarm,
        arguments: args,
      });
      const outcome = typeof data?.outcome === 'string' ? data.outcome : '';
      return {
        text: typeof data?.text === 'string' ? data.text : '',
        data: data?.data,
        outcome,
        unconfirmed: unknownHeader(headers) || UNKNOWN_OUTCOMES.has(outcome),
      };
    } catch (error) {
      if (error instanceof BroodApiError) {
        throw new CommandError(error, unknownHeader(error.headers));
      }
      throw error;
    }
  },
};

/** Одна запись истории сессии в виде для чтения человеком. */
export interface HistoryEntry {
  kind: 'user' | 'assistant' | 'tool' | 'summary' | 'system' | 'notice' | 'other';
  time?: string;
  text: string;
}

export interface HistoryPage {
  epoch: string;
  /** Непрозрачный курсор следующей страницы. */
  cursor: string;
  /** Transcript сменился: прежние записи к этой истории не относятся. */
  reset: boolean;
  hasMore: boolean;
  partial: boolean;
  entries: HistoryEntry[];
}

export interface UsageDay {
  day: string;
  input: number;
  output: number;
  cacheWrite: number;
  cacheRead: number;
}

export interface UsageReport {
  days: number;
  input: number;
  output: number;
  cacheWrite: number;
  cacheRead: number;
  partial: boolean;
  files: number;
  scope: string;
  perDay: UsageDay[];
}

const MAX_ENTRY = 2000;

const clip = (text: string) => (text.length > MAX_ENTRY ? `${text.slice(0, MAX_ENTRY)}…` : text);

const isObject = (value: unknown): value is Record<string, unknown> =>
  typeof value === 'object' && value !== null && !Array.isArray(value);

// Текст блоков содержимого Claude: text — как есть, вызов инструмента —
// его имя, результат — его текст. Рассуждения (thinking) не показываются.
function blocksText(content: unknown): { text: string; tools: boolean; results: boolean } {
  if (typeof content === 'string') return { text: content, tools: false, results: false };
  const parts: string[] = [];
  let tools = false;
  let results = false;
  for (const block of Array.isArray(content) ? content : []) {
    if (!isObject(block)) continue;
    if (block.type === 'text' && typeof block.text === 'string') parts.push(block.text);
    if (block.type === 'tool_use') {
      tools = true;
      parts.push(`→ ${typeof block.name === 'string' ? block.name : 'tool'}`);
    }
    if (block.type === 'tool_result') {
      results = true;
      parts.push(blocksText(block.content).text);
    }
  }
  return { text: parts.filter(Boolean).join('\n'), tools, results };
}

/** Запись transcript (JSONL Claude Code) или notice сервера -> запись для
 * чтения; null — запись без видимого содержимого (например, только thinking). */
export function historyEntry(record: unknown): HistoryEntry | null {
  if (!isObject(record) || typeof record.type !== 'string') {
    return { kind: 'notice', text: 'invalid_record' };
  }
  const time = typeof record.timestamp === 'string' ? record.timestamp : undefined;
  const at = (entry: HistoryEntry): HistoryEntry => (time ? { ...entry, time } : entry);
  const message = isObject(record.message) ? record.message : {};
  switch (record.type) {
    case 'user': {
      const { text, results } = blocksText(message.content);
      return text ? at({ kind: results ? 'tool' : 'user', text: clip(text) }) : null;
    }
    case 'assistant': {
      const { text } = blocksText(message.content);
      return text ? at({ kind: 'assistant', text: clip(text) }) : null;
    }
    case 'summary':
      return at({ kind: 'summary', text: clip(String(record.summary ?? '')) });
    case 'system':
      return at({ kind: 'system', text: clip(String(record.content ?? 'system')) });
    case 'brood_history_notice':
      return at({ kind: 'notice', text: String(record.reason ?? 'notice') });
    default:
      return at({ kind: 'other', text: record.type });
  }
}

function historyPage(data: unknown): HistoryPage {
  if (
    !isObject(data) ||
    typeof data.cursor !== 'string' ||
    typeof data.epoch !== 'string' ||
    !Array.isArray(data.records)
  ) {
    throw new Error('Server returned an invalid session history page');
  }
  return {
    epoch: data.epoch,
    cursor: data.cursor,
    reset: data.reset === true,
    hasMore: data.has_more === true,
    partial: data.partial === true,
    entries: data.records.map(historyEntry).filter((e): e is HistoryEntry => e !== null),
  };
}

const count = (value: unknown) =>
  typeof value === 'number' && Number.isFinite(value) && value >= 0 ? value : null;

function usageReport(data: unknown): UsageReport {
  const fields = isObject(data)
    ? [data.days, data.input, data.output, data.cache_write, data.cache_read].map(count)
    : [null];
  if (!isObject(data) || fields.some((value) => value === null)) {
    throw new Error('Server returned an invalid usage report');
  }
  const [days, input, output, cacheWrite, cacheRead] = fields as number[];
  const perDay = Object.entries(isObject(data.per_day) ? data.per_day : {})
    .filter((entry): entry is [string, number[]] => Array.isArray(entry[1]))
    .map(([day, v]) => ({
      day,
      input: count(v[0]) ?? 0,
      output: count(v[1]) ?? 0,
      cacheWrite: count(v[2]) ?? 0,
      cacheRead: count(v[3]) ?? 0,
    }))
    .sort((a, b) => b.day.localeCompare(a.day));
  return {
    days,
    input,
    output,
    cacheWrite,
    cacheRead,
    partial: data.partial === true,
    files: count(data.files) ?? 0,
    scope: typeof data.scope === 'string' ? data.scope : '',
    perDay,
  };
}

const mutation = (tool: 'restart' | 'recycle' | 'wipe') => (swarm: string, name: string) =>
  commandsApi.run(tool, swarm, { name, confirm: true });

/** Сессия и workspace одного managed-актора выбранного роя. */
export const sessionApi = {
  restart: mutation('restart'),
  recycle: mutation('recycle'),
  wipe: mutation('wipe'),
  /** Снимок экрана tmux: текст как есть, даже если он похож на JSON. */
  tail: async (swarm: string, name: string, { lines, grep }: { lines: number; grep: string }) =>
    (await commandsApi.run('tail', swarm, { name, lines, ...(grep ? { grep } : {}) })).text,
  history: async (
    swarm: string,
    name: string,
    { cursor, limit }: { cursor: string; limit: number }
  ) =>
    historyPage(
      (await commandsApi.run('session_history', swarm, { name, ...(cursor ? { cursor } : {}), limit }))
        .data
    ),
  stat: async (swarm: string, name: string, days: number) =>
    usageReport((await commandsApi.run('stat', swarm, { name, days })).data),
  /** Квитанция доставки, не ответ модели. */
  send: (swarm: string, to: string, message: string, waitSeconds: number) =>
    commandsApi.run('send', swarm, { to, message, wait_seconds: waitSeconds }),
  slash: (swarm: string, name: string, command: string) =>
    commandsApi.run('slash', swarm, { name, command }),
};
