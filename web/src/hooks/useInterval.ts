/**
 * Таймеры опроса. useInterval тикает, пока задержка не null; useVisibleInterval
 * ещё и молчит в скрытой вкладке (#257): фоновая вкладка не нагружает сервер,
 * а при возвращении тикает сразу, чтобы не показывать устаревшее до
 * следующего периода.
 */

import { useEffect, useRef, useState } from 'react';

export function useInterval(callback: () => void, delay: number | null) {
  const savedCallback = useRef<(() => void) | null>(null);

  useEffect(() => {
    savedCallback.current = callback;
  }, [callback]);

  useEffect(() => {
    if (delay === null) return;

    const tick = () => {
      savedCallback.current?.();
    };

    const id = setInterval(tick, delay);
    return () => clearInterval(id);
  }, [delay]);
}

/** Видна ли вкладка. Без document (серверный рендер) — видна. */
const documentVisible = () => typeof document === 'undefined' || !document.hidden;

/** Видимость вкладки документа, по событию visibilitychange. */
export function useDocumentVisible(): boolean {
  const [visible, setVisible] = useState(documentVisible);
  useEffect(() => {
    if (typeof document === 'undefined') return;
    const update = () => setVisible(documentVisible());
    update();
    document.addEventListener('visibilitychange', update);
    return () => document.removeEventListener('visibilitychange', update);
  }, []);
  return visible;
}

/** Интервал, который останавливается в скрытой вкладке и, когда вкладку
 * показали снова, тикает один раз сразу, а дальше по периоду. */
export function useVisibleInterval(callback: () => void, delay: number | null) {
  const visible = useDocumentVisible();
  const savedCallback = useRef(callback);
  useEffect(() => {
    savedCallback.current = callback;
  }, [callback]);
  // Возврат во вкладку: тик сразу. Первый рендер — не возврат.
  const wasHidden = useRef(false);
  useEffect(() => {
    if (!visible) {
      wasHidden.current = true;
      return;
    }
    if (wasHidden.current && delay !== null) savedCallback.current();
    wasHidden.current = false;
  }, [visible, delay]);
  useInterval(callback, visible ? delay : null);
}
