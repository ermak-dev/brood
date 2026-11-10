/**
 * Запросы оператору страницы роя (#216): одно состояние на страницу —
 * его рисуют и инбокс, и карточка у выбранного узла графа. Ответ и повтор
 * доставки идут в API запросов роя (#210); итог действия остаётся у
 * запроса, где бы ни стояла его карточка.
 */
import { useCallback, useEffect, useRef, useState } from 'react';
import { useHeaderRefresh } from '@/hooks/useHeaderRefresh';
import type { AccessSwarm } from '@/services/brood/access';
import { errorMessage } from '@/services/brood/http';
import { requestsApi, type RequestAnswer, type SwarmRequest } from '@/services/brood/requests';
import { canAnswerRequests } from './requestState';
import { useSwarmTick } from './useSwarmPolling';

/** Где показана карточка: в инбоксе или у выбранного узла графа. */
export type RequestPlace = 'inbox' | 'node';

/** Итог последнего действия над запросом: подтверждение ответа, отказ
 * сервера его причиной, причина неудачной доставки. Живёт до следующего
 * действия над тем же запросом, где бы ни стояла его карточка. */
export interface RequestNotice {
  answered?: boolean;
  refusal?: string;
  deliveryError?: string;
}

export interface RequestFocus {
  id: string;
  place: RequestPlace;
}

/** Состояние запросов роя и действия над ними: одно на страницу, его
 * рисуют и инбокс, и карточка у выбранного узла. */
export interface RequestInboxModel {
  /** Все запросы роя; null — ещё не прочитаны. */
  requests: SwarmRequest[] | null;
  error: string;
  refresh: () => Promise<void>;
  canAnswer: boolean;
  notices: Record<string, RequestNotice>;
  /** Подтверждение, которому перейти фокус после отправки. */
  focus: RequestFocus | null;
  focused: () => void;
  answer: (request: SwarmRequest, body: RequestAnswer, place: RequestPlace) => Promise<void>;
  deliver: (request: SwarmRequest) => Promise<void>;
}

export function useSwarmRequests(
  swarmId: string,
  swarm: AccessSwarm | null,
  onAnswered?: (request: SwarmRequest, place: RequestPlace) => void
): RequestInboxModel {
  const [requests, setRequests] = useState<SwarmRequest[] | null>(null);
  const [error, setError] = useState('');
  const [notices, setNotices] = useState<Record<string, RequestNotice>>({});
  const [focus, setFocus] = useState<RequestFocus | null>(null);
  // Номер последнего чтения: чтение, начатое до ответа, не возвращает
  // отвеченный запрос в открытые.
  const latest = useRef(0);

  const refresh = useCallback(async () => {
    const call = ++latest.current;
    try {
      const loaded = await requestsApi.list(swarmId);
      if (call !== latest.current) return;
      setRequests(loaded);
      setError('');
    } catch (failure) {
      if (call !== latest.current) return;
      setError(errorMessage(failure));
    }
  }, [swarmId]);

  useEffect(() => {
    void refresh();
  }, [refresh]);
  useHeaderRefresh(refresh);
  // Запросы перечитываются каждым тиком страницы (#257), пока она опрашивает:
  // вопрос актора приходит без изменения графа, и ждать его до ручного
  // обновления нельзя.
  useSwarmTick(refresh);

  // Ответ сервера на действие — самое свежее состояние запроса.
  const settle = useCallback((request: SwarmRequest) => {
    latest.current += 1;
    setRequests((list) => list?.map((item) => (item.id === request.id ? request : item)) ?? list);
  }, []);
  const note = useCallback((id: string, notice: RequestNotice) => {
    setNotices((all) => ({ ...all, [id]: notice }));
  }, []);
  const focused = useCallback(() => setFocus(null), []);

  const answer = async (request: SwarmRequest, body: RequestAnswer, place: RequestPlace) => {
    note(request.id, {});
    try {
      const recorded = await requestsApi.answer(swarmId, request.id, body);
      settle(recorded);
      note(request.id, { answered: true, deliveryError: recorded.deliveryError });
      setFocus({ id: request.id, place });
      onAnswered?.(recorded, place);
    } catch (failure) {
      // Отказ остаётся у запроса, а перечитывание показывает, чем он
      // закрыт на самом деле: например, ответом другого управляющего.
      note(request.id, { refusal: errorMessage(failure) });
    }
    void refresh();
  };

  const deliver = async (request: SwarmRequest) => {
    note(request.id, {});
    try {
      const recorded = await requestsApi.deliver(swarmId, request.id);
      settle(recorded);
      note(request.id, { deliveryError: recorded.deliveryError });
    } catch (failure) {
      note(request.id, { refusal: errorMessage(failure) });
    }
    void refresh();
  };

  return {
    requests,
    error,
    refresh,
    canAnswer: canAnswerRequests(swarm),
    notices,
    focus,
    focused,
    answer,
    deliver,
  };
}
