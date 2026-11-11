/**
 * 媒体查询 Hook
 */

import { useState, useEffect } from 'react';

/** Без matchMedia (серверный рендер, тестовый DOM) запрос не совпадает. */
const canMatch = () => typeof window !== 'undefined' && typeof window.matchMedia === 'function';

export function useMediaQuery(query: string): boolean {
  const [matches, setMatches] = useState(() => {
    return canMatch() && window.matchMedia(query).matches;
  });

  useEffect(() => {
    if (!canMatch()) return undefined;
    const media = window.matchMedia(query);

    const listener = (event: MediaQueryListEvent) => {
      setMatches(event.matches);
    };

    // Set initial value via listener to avoid direct setState in effect
    listener({ matches: media.matches } as MediaQueryListEvent);

    media.addEventListener('change', listener);
    return () => media.removeEventListener('change', listener);
  }, [query]);

  return matches;
}
