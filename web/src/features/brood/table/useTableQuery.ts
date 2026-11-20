import { useCallback, useMemo } from 'react';
import { useSearchParams } from 'react-router-dom';

export interface SortState {
  column: string;
  desc: boolean;
}

/** State of a table kept in the query string, so a filtered and sorted view
 * is a link and survives a reload: `q` is the text filter, `sort` and `dir`
 * the order, any other name a parent filter. Defaults are not written. */
export function useTableQuery(defaultSort: SortState) {
  const [params, setParams] = useSearchParams();
  const set = useCallback(
    (changes: Record<string, string>) =>
      setParams(
        (current) => {
          const next = new URLSearchParams(current);
          for (const [name, value] of Object.entries(changes)) {
            if (value) next.set(name, value);
            else next.delete(name);
          }
          return next;
        },
        { replace: true }
      ),
    [setParams]
  );
  const sort = useMemo<SortState>(() => {
    const column = params.get('sort');
    return column ? { column, desc: params.get('dir') === 'desc' } : defaultSort;
  }, [params, defaultSort]);
  const setSort = useCallback(
    (next: SortState) =>
      set(
        next.column === defaultSort.column && next.desc === defaultSort.desc
          ? { sort: '', dir: '' }
          : { sort: next.column, dir: next.desc ? 'desc' : '' }
      ),
    [set, defaultSort]
  );
  return { get: (name: string) => params.get(name) ?? '', set, sort, setSort };
}
