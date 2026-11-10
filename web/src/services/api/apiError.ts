import { isRecord } from '@/utils/helpers';

export interface ParsedApiErrorResponse {
  message: string;
  apiCode?: string;
}

const readString = (value: unknown): string =>
  typeof value === 'string' ? value.trim() : '';

/** Тело, прочитанное как текст (config.yaml идёт с responseType text),
 * может быть JSON-конвертом отказа: разбираем его, а не показываем сырым. */
const parseTextBody = (body: string): unknown => {
  const text = body.trim();
  if (!text.startsWith('{')) return body;
  try {
    return JSON.parse(text);
  } catch {
    return body;
  }
};

/**
 * Parse the Management API's error envelope.
 *
 * Newer endpoints use `error` as a stable machine-readable code and `message`
 * as the human-readable detail. Older endpoints may put the only useful text
 * directly in `error`, so that remains a fallback. The Go API of brood answers
 * refusals with `detail` ({"detail": "<reason>"}), including the 503 of an
 * unconfigured LLM backend behind the management proxy.
 */
export const parseApiErrorResponse = (
  responseData: unknown,
  fallbackMessage: string
): ParsedApiErrorResponse => {
  const data = typeof responseData === 'string' ? parseTextBody(responseData) : responseData;
  if (!isRecord(data)) {
    return {
      message: readString(data) || readString(fallbackMessage) || 'Request failed',
    };
  }

  const errorValue = data.error;
  const errorRecord = isRecord(errorValue) ? errorValue : null;
  const stringError = readString(errorValue);
  const apiCode = stringError || readString(errorRecord?.code) || undefined;
  const message =
    readString(data.message) ||
    readString(errorRecord?.message) ||
    readString(data.detail) ||
    stringError ||
    readString(fallbackMessage) ||
    'Request failed';

  return { message, apiCode };
};
