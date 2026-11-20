import {
  Fragment,
  useEffect,
  useRef,
  useState,
  type KeyboardEvent as ReactKeyboardEvent,
  type MouseEvent as ReactMouseEvent,
  type ReactNode,
} from 'react';
import { Link, useSearchParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import {
  IconChevronDown,
  IconChevronUp,
  IconPlus,
  IconRefreshCw,
  IconSearch,
  IconX,
} from '@/components/ui/icons';
import { Select, type SelectOption } from '@/components/ui/Select';
import { Skeleton } from '@/components/ui/Skeleton';
import { errorMessage } from '@/services/brood/http';
import { useNotificationStore } from '@/stores';
import type { Resource } from '../hooks';
import { IconChevronRight, IconHelp } from '../icons';
import { PageHeader, type BadgeTone, type Icon } from '../ui';
import { ConfirmDialog, type Confirmation } from './dialogs';
import { EntityPanel, type PanelAction, type PanelSpec } from './EntityPanel';
import { useTableQuery, type SortState } from './useTableQuery';
import styles from './table.module.scss';

export interface Column<T> {
  id: string;
  header: string;
  cell: (row: T) => ReactNode;
  /** What the text filter matches in this column. */
  text?: (row: T) => string;
  /** The sort key; a column without one is not sortable. */
  sort?: (row: T) => string | number;
  numeric?: boolean;
  /** The name of the row: bold, and the title of its card on a phone. */
  primary?: boolean;
}

/** A filter of the table; its value lives in the query under `param`. */
export interface TableFilter<T> {
  param: string;
  label: string;
  /** The option that shows every row. */
  all: string;
  options: SelectOption[];
  value: (row: T) => string;
  /** A filter the summary strip counts by: the strip sees the rows before
   * it, so its numbers do not collapse to the chosen one. */
  facet?: boolean;
}

/** One step of getting from an empty installation to a working agent. */
export interface GuideStep {
  id: string;
  label: string;
  /** Where the step is done; absent when it is done outside the panel. */
  to?: string;
  /** The step this page is about. */
  current?: boolean;
}

const collator = new Intl.Collator(undefined, { numeric: true, sensitivity: 'base' });

const compare = (a: string | number, b: string | number) =>
  typeof a === 'number' && typeof b === 'number' ? a - b : collator.compare(String(a), String(b));

/** Typing somewhere, or a sheet or dialog on top: the page keys stay out. */
const keysBelongElsewhere = (target: EventTarget | null) =>
  (target instanceof HTMLElement &&
    (target.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(target.tagName))) ||
  document.querySelector('[role="dialog"], [role="listbox"]') !== null;

/** A count of the summary strip that is also a one-click filter: a click
 * narrows the table to it, a second click shows everything again. */
export function SummaryChip({
  param,
  value,
  label,
  count,
  icon: Mark,
  tone,
}: {
  param: string;
  value: string;
  label: string;
  count: number;
  icon: Icon;
  tone: BadgeTone;
}) {
  const [params, setParams] = useSearchParams();
  const active = params.get(param) === value;
  const toggle = () =>
    setParams(
      (current) => {
        const next = new URLSearchParams(current);
        if (active) next.delete(param);
        else next.set(param, value);
        return next;
      },
      { replace: true }
    );
  return (
    <button
      type="button"
      className={`${styles.chip} ${active ? styles.chipActive : ''}`}
      data-tone={tone}
      aria-pressed={active}
      onClick={toggle}
    >
      <span className={styles.chipIcon}>
        <Mark size={14} />
      </span>
      <span className={styles.chipCount}>{count}</span>
      <span className={styles.chipLabel}>{label}</span>
    </button>
  );
}

/** One entity, one flat table. A row is a single target that opens the
 * side panel of its object, where the actions live; the open panel is part
 * of the address. Above the rows: the header with the primary action, the
 * summary strip, the filter bar kept in the query string and the sortable
 * heads. On a phone the rows are cards. The create sheet is part of the
 * address too; the page's other sheets come as children. */
export function TablePage<T>({
  title,
  icon,
  description,
  create,
  resource,
  rowKey,
  rowName,
  columns,
  filters = [],
  summary,
  quick,
  panel,
  defaultSort,
  empty,
  guide = [],
  afterAction,
  children,
}: {
  title: string;
  icon: Icon;
  description: string;
  /** The primary action and its sheet; absent when the user may not create
   * the entity. */
  create?: {
    label: string;
    sheet: (dialog: { open: boolean; close: () => void }) => ReactNode;
  };
  resource: Resource<T[]>;
  rowKey: (row: T) => string;
  rowName: (row: T) => string;
  columns: Column<T>[];
  filters?: TableFilter<T>[];
  /** The strip above the table, from the rows before the facet filters. */
  summary?: (rows: T[]) => ReactNode;
  /** The one action people come to the page for, inline in the row. */
  quick?: { header: string; cell: (row: T) => ReactNode };
  panel: (row: T) => PanelSpec;
  defaultSort: SortState;
  /** One sentence on what the entity is, for the empty table. */
  empty: string;
  /** What to do next, in order, when there is nothing yet. */
  guide?: GuideStep[];
  /** Runs after every mutation instead of the table refresh. */
  afterAction?: () => unknown;
  children?: ReactNode;
}) {
  const { t } = useTranslation();
  const notify = useNotificationStore((state) => state.showNotification);
  const query = useTableQuery(defaultSort);
  const search = useRef<HTMLInputElement | null>(null);
  const [confirmation, setConfirmation] = useState<Confirmation | null>(null);
  /** The row and the action with a mutation in flight. */
  const [pending, setPending] = useState<{ row: string; action: string } | null>(null);
  const [keysShown, setKeysShown] = useState(false);

  const all = resource.data;
  const text = query.get('q');
  const { sort } = query;
  const needle = text.trim().toLowerCase();
  const sortKey = columns.find((column) => column.id === sort.column)?.sort;
  const passes = (row: T, filter: TableFilter<T>) => {
    const wanted = query.get(filter.param);
    return !wanted || filter.value(row) === wanted;
  };
  const scoped = (all ?? [])
    .filter((row) => filters.every((filter) => filter.facet || passes(row, filter)))
    .filter(
      (row) =>
        !needle ||
        columns.some((column) => column.text && column.text(row).toLowerCase().includes(needle))
    );
  const rows = scoped
    .filter((row) => filters.every((filter) => !filter.facet || passes(row, filter)))
    .sort((a, b) => {
      const order = sortKey ? compare(sortKey(a), sortKey(b)) : 0;
      return (sort.desc ? -order : order) || collator.compare(rowName(a), rowName(b));
    });

  const filtered = text !== '' || filters.some((filter) => query.get(filter.param) !== '');
  const clear = () =>
    query.set({ q: '', ...Object.fromEntries(filters.map((filter) => [filter.param, ''])) });

  // ─── the open panel ────────────────────────────────────────────────
  const openId = query.get('open');
  const current = (openId && all?.find((row) => rowKey(row) === openId)) || null;
  // The last object stays on screen while the sheet plays its closing; a
  // refresh swaps the object under an open panel without remounting it.
  const [shown, setShown] = useState<T | null>(null);
  if (current && current !== shown) setShown(current);
  const known = shown !== null && rowKey(shown) === openId;
  const gone = openId !== '' && all !== null && current === null;
  const { close } = query;
  useEffect(() => {
    if (!gone) return;
    // An object deleted under its panel just closes it; a link to one that
    // does not exist says so.
    if (!known) notify(t('brood.table.not_found', { name: openId }), 'error');
    close();
  }, [gone, known, openId, close, notify, t]);

  // ─── the create sheet ──────────────────────────────────────────────
  // Open while the address says so: a link can ask for it, Back closes it,
  // and nothing is lost if the page remounts. Every opening is a new
  // session, so the form starts clean.
  const creating = create !== undefined && query.get('create') !== '';
  const [session, setSession] = useState({ count: 0, open: false });
  if (creating !== session.open)
    setSession({ count: session.count + (creating ? 1 : 0), open: creating });
  const startCreate = create ? query.startCreate : undefined;
  const createRef = useRef(startCreate);
  useEffect(() => {
    createRef.current = startCreate;
  });

  // ─── keys of the page ──────────────────────────────────────────────
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.defaultPrevented || event.ctrlKey || event.metaKey || event.altKey) return;
      if (keysBelongElsewhere(event.target)) return;
      if (event.key === '/') {
        event.preventDefault();
        search.current?.focus();
        search.current?.select();
      } else if (event.key === 'n' && createRef.current) {
        event.preventDefault();
        createRef.current();
      } else if (event.key === '?') {
        event.preventDefault();
        setKeysShown((prev) => !prev);
      } else if (event.key === 'Escape') {
        setKeysShown(false);
      }
    };
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, []);

  const focusRow = (from: HTMLElement | null, step: 'first' | 'last' | 1 | -1) => {
    const list = Array.from(document.querySelectorAll<HTMLElement>('tr[data-row]'));
    const index = from ? list.indexOf(from) : -1;
    const next =
      step === 'first'
        ? list[0]
        : step === 'last'
          ? list[list.length - 1]
          : list[Math.min(list.length - 1, Math.max(0, index + step))];
    next?.focus();
  };
  const onRowKey = (event: ReactKeyboardEvent<HTMLTableRowElement>, row: T) => {
    if (event.target !== event.currentTarget) return;
    const steps: Record<string, 'first' | 'last' | 1 | -1> = {
      ArrowDown: 1,
      ArrowUp: -1,
      Home: 'first',
      End: 'last',
    };
    if (event.key === 'Enter' || event.key === ' ') query.open(rowKey(row));
    else if (event.key in steps) focusRow(event.currentTarget, steps[event.key]);
    else return;
    event.preventDefault();
  };
  const onRowClick = (event: ReactMouseEvent<HTMLTableRowElement>, row: T) => {
    // A link or a button inside the row acts for itself.
    if ((event.target as HTMLElement).closest('a, button')) return;
    query.open(rowKey(row));
  };

  // ─── actions of the panel ──────────────────────────────────────────
  const mutate = async (row: T, action: PanelAction) => {
    setPending({ row: rowKey(row), action: action.id });
    try {
      await action.run?.();
      notify(
        action.notice ?? t('brood.table.done', { action: action.label, name: rowName(row) }),
        'success'
      );
    } finally {
      setPending(null);
      void (afterAction ?? resource.refresh)();
    }
  };
  const select = (row: T, action: PanelAction) => {
    if (action.onSelect) action.onSelect();
    else if (action.confirm)
      setConfirmation({
        title: t('brood.table.confirm_title', { action: action.label, name: rowName(row) }),
        message: action.confirm,
        confirmLabel: action.label,
        action: () => mutate(row, action),
      });
    else void mutate(row, action).catch((error) => notify(errorMessage(error), 'error'));
  };
  const subject = current ?? shown;

  const heads = (
    <thead>
      <tr>
        {columns.map((column) => {
          const active = sort.column === column.id;
          const direction = active ? (sort.desc ? 'descending' : 'ascending') : 'none';
          return (
            <th
              key={column.id}
              className={column.numeric ? styles.numeric : undefined}
              aria-sort={column.sort ? direction : undefined}
            >
              {column.sort ? (
                <button
                  type="button"
                  className={`${styles.sortButton} ${active ? styles.sortActive : ''}`}
                  title={t('brood.table.sort_by', { column: column.header })}
                  onClick={() =>
                    query.setSort({ column: column.id, desc: active ? !sort.desc : false })
                  }
                >
                  {column.header}
                  <span className={styles.sortMark} aria-hidden="true">
                    {active && sort.desc ? (
                      <IconChevronDown size={12} />
                    ) : (
                      <IconChevronUp size={12} />
                    )}
                  </span>
                </button>
              ) : (
                column.header
              )}
            </th>
          );
        })}
        {quick && (
          <th>
            <span className={styles.srOnly}>{quick.header}</span>
          </th>
        )}
        <th className={styles.cellOpen} aria-hidden="true" />
      </tr>
    </thead>
  );

  const createButton = create && (
    <Button onClick={startCreate}>
      <span className={styles.withIcon}>
        <IconPlus size={16} />
        {create.label}
      </span>
    </Button>
  );

  const strip = summary?.(scoped);
  let body: ReactNode;
  if (all === null && !resource.error) {
    body = (
      <div className={styles.tableWrap} aria-busy="true" aria-label={t('brood.table.loading')}>
        <table className={styles.table}>
          {heads}
          <tbody>
            {[0, 1, 2, 3].map((line) => (
              <tr key={line}>
                {columns.map((column) => (
                  <td key={column.id}>
                    <Skeleton width="70%" height={14} />
                  </td>
                ))}
                {quick && <td />}
                <td className={styles.cellOpen} />
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    );
  } else if (all === null) {
    body = (
      <div className={styles.loadFailure} role="alert">
        <p>{t('brood.table.load_failed', { error: resource.error })}</p>
        <Button variant="secondary" size="sm" onClick={() => void resource.refresh()}>
          <span className={styles.withIcon}>
            <IconRefreshCw size={14} />
            {t('brood.table.retry')}
          </span>
        </Button>
      </div>
    );
  } else if (all.length === 0) {
    const Mark = icon;
    body = (
      <div className={styles.empty}>
        <span className={styles.emptyIcon}>
          <Mark size={24} />
        </span>
        <h2 className={styles.emptyTitle}>{t('brood.table.empty')}</h2>
        <p className={styles.emptyText}>{empty}</p>
        {guide.length > 0 && (
          <ol className={styles.guide} aria-label={t('brood.guide.title')}>
            {guide.map((step) => (
              <li key={step.id} aria-current={step.current ? 'step' : undefined}>
                {step.to && !step.current ? <Link to={step.to}>{step.label}</Link> : step.label}
              </li>
            ))}
          </ol>
        )}
        {createButton}
      </div>
    );
  } else {
    body = (
      <>
        {strip && <div className={styles.summary}>{strip}</div>}
        <div className={styles.filters} role="search">
          <label className={styles.search}>
            <span className={styles.srOnly}>{t('brood.table.filter')}</span>
            <IconSearch size={15} aria-hidden="true" />
            <input
              ref={search}
              className="input"
              type="search"
              value={text}
              placeholder={t('brood.table.filter')}
              autoComplete="off"
              spellCheck={false}
              onChange={(event) => query.set({ q: event.target.value })}
              onKeyDown={(event) => {
                if (event.key !== 'ArrowDown') return;
                event.preventDefault();
                focusRow(null, 'first');
              }}
            />
          </label>
          {filters.map((filter) => {
            const value = query.get(filter.param);
            const known = filter.options.some((option) => option.value === value);
            return (
              <Select
                key={filter.param}
                className={styles.filterSelect}
                fullWidth={false}
                size="sm"
                ariaLabel={filter.label}
                value={value}
                options={[
                  { value: '', label: filter.all },
                  ...filter.options,
                  ...(value && !known ? [{ value, label: value }] : []),
                ]}
                onChange={(next) => query.set({ [filter.param]: next })}
              />
            );
          })}
          {filtered && (
            <Button variant="ghost" size="sm" onClick={clear}>
              <span className={styles.withIcon}>
                <IconX size={14} />
                {t('brood.table.clear_filters')}
              </span>
            </Button>
          )}
          <span className={styles.count} aria-live="polite">
            {filtered
              ? t('brood.table.shown', { shown: rows.length, total: all.length })
              : t('brood.table.total', { count: all.length })}
          </span>
          <span className={styles.keys}>
            <button
              type="button"
              className={styles.keysButton}
              aria-label={t('brood.keys.title')}
              title={t('brood.keys.title')}
              aria-expanded={keysShown}
              onClick={() => setKeysShown(!keysShown)}
            >
              <IconHelp size={16} />
            </button>
            {keysShown && (
              <dl className={styles.keysList}>
                {(
                  [
                    ['/', 'filter'],
                    ...(create ? [['n', 'create']] : []),
                    ['↑ ↓', 'move'],
                    ['Enter', 'open'],
                    ['Esc', 'close'],
                    ['?', 'help'],
                  ] as [string, string][]
                ).map(([key, id]) => (
                  <div key={id}>
                    <dt>
                      <kbd>{key}</kbd>
                    </dt>
                    <dd>{t(`brood.keys.${id}`)}</dd>
                  </div>
                ))}
              </dl>
            )}
          </span>
        </div>
        {resource.error && (
          <p className={styles.failure} role="alert">
            {t('brood.table.refresh_failed', { error: resource.error })}
          </p>
        )}
        {rows.length === 0 ? (
          <div className={styles.empty}>
            <span className={styles.emptyIcon}>
              <IconSearch size={24} />
            </span>
            <h2 className={styles.emptyTitle}>{t('brood.table.no_match')}</h2>
            <p className={styles.emptyText}>{t('brood.table.no_match_hint')}</p>
            <Button variant="secondary" size="sm" onClick={clear}>
              {t('brood.table.clear_filters')}
            </Button>
          </div>
        ) : (
          <div className={styles.tableWrap}>
            <table className={styles.table}>
              {heads}
              <tbody>
                {rows.map((row) => {
                  const key = rowKey(row);
                  return (
                    <tr
                      key={key}
                      data-row=""
                      className={`${styles.row} ${key === openId ? styles.rowActive : ''}`}
                      tabIndex={0}
                      onClick={(event) => onRowClick(event, row)}
                      onKeyDown={(event) => onRowKey(event, row)}
                    >
                      {columns.map((column) => (
                        <td
                          key={column.id}
                          className={
                            [column.numeric && styles.numeric, column.primary && styles.cellPrimary]
                              .filter(Boolean)
                              .join(' ') || undefined
                          }
                          data-label={column.header}
                        >
                          {column.cell(row)}
                        </td>
                      ))}
                      {quick && <td className={styles.cellQuick}>{quick.cell(row)}</td>}
                      <td className={styles.cellOpen} aria-hidden="true">
                        <IconChevronRight size={16} />
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </>
    );
  }

  return (
    <div className={styles.page}>
      <PageHeader title={title} icon={icon} description={description} actions={createButton} />
      {body}
      <EntityPanel
        open={current !== null}
        close={close}
        spec={subject ? panel(subject) : null}
        pending={subject && pending?.row === rowKey(subject) ? pending.action : null}
        busy={t('brood.table.busy')}
        select={(action) => subject && select(subject, action)}
      />
      {create && session.count > 0 && (
        <Fragment key={session.count}>
          {create.sheet({ open: creating, close: query.endCreate })}
        </Fragment>
      )}
      {children}
      <ConfirmDialog request={confirmation} close={() => setConfirmation(null)} />
    </div>
  );
}
