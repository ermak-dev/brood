import { useCallback, useEffect, useRef, useState } from 'react';
import { errorMessage } from '@/services/brood/http';

interface ReadState<T> {
  data: T | null;
  error: string;
  loading: boolean;
}

/** brood: чтение данных сессии по требованию. Ответ на прежний запрос,
 * пришедший позже нового, не ложится поверх; после закрытия деталей ответы
 * отбрасываются. apply сводит ответ с уже загруженными данными (страницы). */
export function useSessionRead<T>() {
  const latest = useRef(0);
  const [state, setState] = useState<ReadState<T>>({ data: null, error: '', loading: false });

  const run = useCallback(
    async <N,>(load: () => Promise<N>, apply: (previous: T | null, next: N) => T) => {
      const request = ++latest.current;
      setState((previous) => ({ ...previous, loading: true, error: '' }));
      try {
        const next = await load();
        if (request !== latest.current) return;
        setState((previous) => ({ data: apply(previous.data, next), error: '', loading: false }));
      } catch (error) {
        if (request !== latest.current) return;
        setState((previous) => ({ ...previous, error: errorMessage(error), loading: false }));
      }
    },
    []
  );

  useEffect(
    () => () => {
      latest.current += 1;
    },
    []
  );

  return { ...state, run };
}

/** Ответ заменяет прежние данные. */
export const replace = <T,>(_previous: T | null, next: T): T => next;
