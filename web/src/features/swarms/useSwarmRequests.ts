/**
 * Запросы оператору страницы роя (#216): одно состояние на страницу —
 * его рисуют и инбокс, и карточка у выбранного узла графа. Отвечают только
 * в инбоксе (#259): карточка узла ведёт к его форме. Ответ и повтор
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

/** Где показана карточка: в инбоксе (с формой ответа) или у выбранного
 * узла графа (без неё). */
export type RequestPlace = 'inbox' | 'node';

/** Итог последнего действия над запросом: подтверждение ответа, отказ
 * сервера его причиной, причина неудачной доставки. Живёт до следующего
 * действия над тем же запросом; показывает его карточка инбокса — там же,
 * где форма (#259). */
export interface RequestNotice {
  answered?: boolean;
  refusal?: string;
  deliveryError?: string;
}

/** Куда перейти фокусу в карточке запроса в инбоксе: к форме ответа (от
 * узла, «Ответить выше») или к итогу отправки — подтверждению или отказу. */
export interface RequestFocus {
  id: string;
  target: 'form' | 'result';
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
  /** Запросы, на которые отвечали на этой странице (ответ принят или
   * отклонён): в инбоксе они остаются на своём месте, а не уходят в
   * свёрнутую историю вместе с итогом. */
  handledHere: string[];
  /** Запросы, доставку ответа которых повторяли на этой странице: в группе
   * недоставленных они остаются и после удачи, до ухода со страницы (#264). */
  retriedHere: string[];
  /** Карточка инбокса, которой забрать фокус, и куда в ней. */
  focus: RequestFocus | null;
  focused: () => void;
  answer: (request: SwarmRequest, body: RequestAnswer) => Promise<void>;
  /** Перевести фокус к форме ответа на запрос в инбоксе. */
  reach: (request: SwarmRequest) => void;
  deliver: (request: SwarmRequest) => Promise<void>;
}

export function useSwarmRequests(
  swarmId: string,
  swarm: AccessSwarm | null,
  onAnswered?: (request: SwarmRequest) => void
): RequestInboxModel {
  const [requests, setRequests] = useState<SwarmRequest[] | null>(null);
  const [error, setError] = useState('');
  const [notices, setNotices] = useState<Record<string, RequestNotice>>({});
  const [handledHere, setHandledHere] = useState<string[]>([]);
  const [retriedHere, setRetriedHere] = useState<string[]>([]);
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
  const reach = useCallback(
    (request: SwarmRequest) => setFocus({ id: request.id, target: 'form' }),
    []
  );

  const answer = async (request: SwarmRequest, body: RequestAnswer) => {
    note(request.id, {});
    // Форма исчезнет, если запрос закрыт: карточка остаётся на месте, фокус
    // переходит к итогу — подтверждению или отказу.
    setHandledHere((ids) => (ids.includes(request.id) ? ids : [...ids, request.id]));
    try {
      const recorded = await requestsApi.answer(swarmId, request.id, body);
      settle(recorded);
      note(request.id, { answered: true, deliveryError: recorded.deliveryError });
      onAnswered?.(recorded);
    } catch (failure) {
      // Отказ остаётся у запроса, а перечитывание показывает, чем он
      // закрыт на самом деле: например, ответом другого управляющего.
      note(request.id, { refusal: errorMessage(failure) });
    }
    setFocus({ id: request.id, target: 'result' });
    void refresh();
  };

  const deliver = async (request: SwarmRequest) => {
    note(request.id, {});
    // Удачный повтор не уносит карточку в свёрнутую историю вместе с
    // фокусом: новое состояние доставки остаётся на месте (#264).
    setRetriedHere((ids) => (ids.includes(request.id) ? ids : [...ids, request.id]));
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
    handledHere,
    retriedHere,
    focus,
    focused,
    answer,
    reach,
    deliver,
  };
}
