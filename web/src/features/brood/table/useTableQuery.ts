import { useCallback, useEffect, useMemo } from 'react';
import { useLocation, useNavigate, useSearchParams } from 'react-router-dom';
import { rememberQuery } from '../routes';

export interface SortState {
  column: string;
  desc: boolean;
}

/** State of a table kept in the query string, so a view is a link and
 * survives a reload: `q` is the text filter, `sort` and `dir` the order,
 * `open` the object in the side panel, `create` the create sheet, any other
 * name a filter. Defaults are not written. Filters replace the history
 * entry; opening a panel or the create sheet adds one, so the browser's
 * Back closes it. */
export function useTableQuery(defaultSort: SortState) {
  const [params, setParams] = useSearchParams();
  const location = useLocation();
  const navigate = useNavigate();

  const query = params.toString();
  useEffect(() => rememberQuery(location.pathname, query), [location.pathname, query]);

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

  /** Puts a sheet of the page into the address as a new history entry, so
   * the browser's Back closes it. A sheet that is already there is replaced
   * in place and keeps what its entry says about its origin. */
  const push = useCallback(
    (name: string, value: string) =>
      setParams(
        (current) => {
          const next = new URLSearchParams(current);
          next.set(name, value);
          return next;
        },
        params.has(name) ? { state: location.state, replace: true } : { state: { pushed: name } }
      ),
    [setParams, params, location.state]
  );
  /** Takes the sheet out of the address: back to the entry that opened it,
   * or, when the page was entered by a link to the sheet, in place. */
  const pushed = (location.state as { pushed?: string } | null)?.pushed;
  const drop = useCallback(
    (name: string) => {
      if (pushed === name) void navigate(-1);
      else set({ [name]: '' });
    },
    [pushed, navigate, set]
  );
  const open = useCallback((id: string) => push('open', id), [push]);
  const close = useCallback(() => drop('open'), [drop]);
  const startCreate = useCallback(() => push('create', '1'), [push]);
  const endCreate = useCallback(() => drop('create'), [drop]);

  return {
    get: (name: string) => params.get(name) ?? '',
    set,
    sort,
    setSort,
    open,
    close,
    startCreate,
    endCreate,
  };
}
