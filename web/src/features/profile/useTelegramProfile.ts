/**
 * Профиль Telegram на странице (#285): чтение, опрос, пока выданный код ждёт
 * /start, и записи. Каждый запрос получает номер; ответ ложится, только если
 * новее последнего легшего: опрос, ушедший до записи, не вернёт поверх неё
 * прежнее состояние. Пока идёт запись, опрос молчит: запись сама отвечает
 * профилем, а сбой записи перечитывает его. Срок кода считается по часам
 * сервера (Date ответа). Токен бота сюда приходит аргументом записи и нигде
 * не хранится.
 */
import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react';
import { createRefreshRegistry, useHeaderRefresh } from '@/hooks/useHeaderRefresh';
import { useVisibleInterval } from '@/hooks/useInterval';
import { errorMessage } from '@/services/brood/http';
import {
  telegramApi,
  type Served,
  type TelegramLinkCode,
  type TelegramProfile,
} from '@/services/brood/telegram';
import { serverClockOffset, waitsForStart, withIssuedCode } from './telegramState';

/** Период опроса, пока код ждёт /start; как у страницы роя. */
export const TELEGRAM_POLL_MS = 5000;

const ticks = createRefreshRegistry();

/** Один тик опроса: тесты зовут его вместо ожидания таймера. */
export const tickTelegramPolling = () => ticks.refresh();

export interface TelegramProfileModel {
  profile: TelegramProfile | null;
  /** Причина последнего несостоявшегося чтения. */
  error: string;
  /** Код, выданный на этой странице: сервер покажет его только раз. */
  code: TelegramLinkCode | null;
  /** Момент последнего чтения или записи по часам сервера (Date ответа):
   * от него считается срок кода, назначенный сервером. */
  now: number;
  /** Идёт запись. */
  busy: boolean;
  refresh: () => Promise<void>;
  saveToken: (token: string) => Promise<TelegramProfile>;
  remove: () => Promise<TelegramProfile>;
  issueCode: () => Promise<TelegramLinkCode>;
}

/** onState зовётся, когда чтение или запись показали другое состояние,
 * чем было на странице: привязка после /start, ошибка от поллера. Первое
 * чтение -- не смена. */
export function useTelegramProfile(
  onState?: (next: TelegramProfile, prev: TelegramProfile) => void
): TelegramProfileModel {
  const [profile, setProfile] = useState<TelegramProfile | null>(null);
  const [error, setError] = useState('');
  const [code, setCode] = useState<TelegramLinkCode | null>(null);
  const [now, setNow] = useState(() => Date.now());
  const [busy, setBusy] = useState(false);
  // Номер последнего начатого запроса и номер того, чей ответ лёг последним.
  const issued = useRef(0);
  const applied = useRef(0);
  const writes = useRef(0);
  const shown = useRef<TelegramProfile | null>(null);
  // Поправка к часам браузера по Date последнего ответа: срок кода назначен
  // часами сервера, а часы браузера могут уйти на минуты.
  const offset = useRef(0);
  const stateRef = useRef(onState);
  useLayoutEffect(() => {
    stateRef.current = onState;
  });

  const apply = useCallback((ticket: number, next: TelegramProfile, date: string | null) => {
    if (ticket < applied.current) return;
    applied.current = ticket;
    const prev = shown.current;
    shown.current = next;
    const received = Date.now();
    offset.current = serverClockOffset(date, received);
    setProfile(next);
    setError('');
    setNow(received + offset.current);
    if (prev && prev.state !== next.state) stateRef.current?.(next, prev);
  }, []);

  const refresh = useCallback(async () => {
    if (writes.current > 0) return;
    const ticket = ++issued.current;
    try {
      const { data, date } = await telegramApi.profile();
      apply(ticket, data, date);
    } catch (e) {
      if (ticket < applied.current) return;
      applied.current = ticket;
      setError(errorMessage(e));
      setNow(Date.now() + offset.current);
    }
  }, [apply]);

  const write = useCallback(
    async <T>(
      run: () => Promise<Served<T>>,
      done: (result: T, date: string | null, ticket: number) => void
    ): Promise<T> => {
      const ticket = ++issued.current;
      writes.current += 1;
      setBusy(true);
      let answered = false;
      try {
        const { data, date } = await run();
        done(data, date, ticket);
        answered = true;
        return data;
      } finally {
        writes.current -= 1;
        setBusy(writes.current > 0);
        // Сбой записи не говорит, что у сервера: 500 приходит и после
        // зафиксированной записи (сверка поллеров, go/CONTRACT.md), обрыв --
        // когда угодно. Страница перечитывает профиль, а не держит прежний.
        if (!answered && writes.current === 0) void refresh();
      }
    },
    [refresh]
  );

  const saveToken = useCallback(
    (token: string) =>
      write(
        () => telegramApi.setToken(token),
        (next, date, ticket) => apply(ticket, next, date)
      ),
    [apply, write]
  );

  const remove = useCallback(
    () =>
      write(
        () => telegramApi.remove(),
        (next, date, ticket) => {
          setCode(null);
          apply(ticket, next, date);
        }
      ),
    [apply, write]
  );

  const issueCode = useCallback(
    () =>
      write(
        () => telegramApi.issueLinkCode(),
        (issuedCode, date, ticket) => {
          setCode(issuedCode);
          if (shown.current) apply(ticket, withIssuedCode(shown.current, issuedCode), date);
        }
      ),
    [apply, write]
  );

  useEffect(() => {
    void refresh();
  }, [refresh]);
  useEffect(() => ticks.subscribe(refresh), [refresh]);
  useHeaderRefresh(refresh);
  // Опрос идёт, пока код ждёт по слову сервера: истечение увидит ответ опроса.
  const waiting = profile !== null && waitsForStart(profile);
  useVisibleInterval(tickTelegramPolling, waiting ? TELEGRAM_POLL_MS : null);

  return { profile, error, code, now, busy, refresh, saveToken, remove, issueCode };
}
