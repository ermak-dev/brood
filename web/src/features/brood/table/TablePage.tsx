import { useState, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { EmptyState } from '@/components/ui/EmptyState';
import { IconChevronDown, IconChevronUp, IconSearch } from '@/components/ui/icons';
import { Select, type SelectOption } from '@/components/ui/Select';
import { Skeleton } from '@/components/ui/Skeleton';
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/Table';
import { errorMessage } from '@/services/brood/http';
import { useNotificationStore } from '@/stores';
import type { Resource } from '../hooks';
import { PageHeader } from '../ui';
import { ConfirmDialog, type Confirmation } from './dialogs';
import { RowMenu } from './RowMenu';
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
}

/** A parent entity as a filter; its value lives in the query under `param`. */
export interface ParentFilter<T> {
  param: string;
  label: string;
  /** The option that shows every row. */
  all: string;
  options: SelectOption[];
  value: (row: T) => string;
}

/** What a row offers. Exactly one of `href` (a link in a new tab), `open`
 * (a dialog of the page) or `run` (a mutation, asked about first when
 * `confirm` is set). */
export interface RowAction {
  id: string;
  label: string;
  /** Shown as a button in the row; the rest go to the overflow menu. */
  inline?: boolean;
  danger?: boolean;
  /** Why it cannot be used now; shown as the tooltip. */
  disabled?: string;
  href?: string;
  open?: () => void;
  run?: () => Promise<unknown>;
  confirm?: string;
}

const collator = new Intl.Collator(undefined, { numeric: true, sensitivity: 'base' });

const compare = (a: string | number, b: string | number) =>
  typeof a === 'number' && typeof b === 'number' ? a - b : collator.compare(String(a), String(b));

/** One entity, one flat table: header with the primary action, a filter
 * bar kept in the query string, sortable columns, row actions with an
 * overflow menu and confirmation, and the loading, failed, empty and
 * nothing-matches states. The page's own dialogs come as children. */
export function TablePage<T>({
  title,
  description,
  primary,
  resource,
  rowKey,
  rowName,
  columns,
  filters = [],
  actions,
  defaultSort,
  empty,
  afterAction,
  children,
}: {
  title: string;
  description: string;
  /** Absent when the user may not create the entity. */
  primary?: { label: string; onClick: () => void };
  resource: Resource<T[]>;
  rowKey: (row: T) => string;
  rowName: (row: T) => string;
  columns: Column<T>[];
  filters?: ParentFilter<T>[];
  actions?: (row: T) => RowAction[];
  defaultSort: SortState;
  /** One sentence on what the entity is, for the empty table. */
  empty: string;
  /** Runs after every mutation instead of the table refresh. */
  afterAction?: () => unknown;
  children?: ReactNode;
}) {
  const { t } = useTranslation();
  const notify = useNotificationStore((state) => state.showNotification);
  const query = useTableQuery(defaultSort);
  const [confirmation, setConfirmation] = useState<Confirmation | null>(null);
  /** Keys of the rows with a mutation in flight. */
  const [pending, setPending] = useState<ReadonlySet<string>>(new Set());

  const all = resource.data;
  const text = query.get('q');
  const { sort } = query;
  const needle = text.trim().toLowerCase();
  const sortKey = columns.find((column) => column.id === sort.column)?.sort;
  const rows = (all ?? [])
    .filter((row) =>
      filters.every((filter) => {
        const wanted = query.get(filter.param);
        return !wanted || filter.value(row) === wanted;
      })
    )
    .filter(
      (row) =>
        !needle ||
        columns.some((column) => column.text && column.text(row).toLowerCase().includes(needle))
    )
    .sort((a, b) => {
      const order = sortKey ? compare(sortKey(a), sortKey(b)) : 0;
      return (sort.desc ? -order : order) || collator.compare(rowName(a), rowName(b));
    });

  const filtered = text !== '' || filters.some((filter) => query.get(filter.param) !== '');
  const clear = () =>
    query.set({ q: '', ...Object.fromEntries(filters.map((filter) => [filter.param, ''])) });

  const mutate = async (row: T, action: RowAction) => {
    const key = rowKey(row);
    setPending((prev) => new Set(prev).add(key));
    try {
      await action.run?.();
      notify(t('brood.table.done', { action: action.label, name: rowName(row) }), 'success');
    } finally {
      setPending((prev) => {
        const next = new Set(prev);
        next.delete(key);
        return next;
      });
      void (afterAction ?? resource.refresh)();
    }
  };
  const start = (row: T, action: RowAction) => {
    if (action.open) action.open();
    else if (action.confirm)
      setConfirmation({
        title: t('brood.table.confirm_title', { action: action.label, name: rowName(row) }),
        message: action.confirm,
        confirmLabel: action.label,
        action: () => mutate(row, action),
      });
    else void mutate(row, action).catch((error) => notify(errorMessage(error), 'error'));
  };

  const rowActions = (row: T) => {
    const name = rowName(row);
    const busy = pending.has(rowKey(row)) ? t('brood.table.busy') : undefined;
    const offered = (actions?.(row) ?? []).map((action) => ({
      ...action,
      disabled: action.disabled ?? busy,
    }));
    return (
      <div className={styles.rowActions}>
        {offered
          .filter((action) => action.inline)
          .map((action) => {
            const label = t('brood.table.action_named', { action: action.label, name });
            return action.href && !action.disabled ? (
              <a
                key={action.id}
                className="btn btn-secondary btn-sm"
                href={action.href}
                target="_blank"
                rel="noopener noreferrer"
                aria-label={label}
              >
                <span>{action.label}</span>
              </a>
            ) : (
              <Button
                key={action.id}
                variant="secondary"
                size="sm"
                className={action.disabled ? styles.unavailable : undefined}
                aria-label={label}
                aria-disabled={action.disabled ? true : undefined}
                title={action.disabled}
                onClick={() => !action.disabled && start(row, action)}
              >
                {action.label}
              </Button>
            );
          })}
        <RowMenu
          label={t('brood.table.more_actions', { name })}
          items={offered
            .filter((action) => !action.inline)
            .map((action) => ({ ...action, select: () => start(row, action) }))}
        />
      </div>
    );
  };

  const heads = (
    <TableHeader>
      <tr>
        {columns.map((column) => {
          const active = sort.column === column.id;
          const direction = active ? (sort.desc ? 'descending' : 'ascending') : 'none';
          return (
            <TableHead
              key={column.id}
              alignRight={column.numeric}
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
            </TableHead>
          );
        })}
        {actions && (
          <TableHead>
            <span className={styles.srOnly}>{t('brood.table.actions')}</span>
          </TableHead>
        )}
      </tr>
    </TableHeader>
  );

  const create = primary && <Button onClick={primary.onClick}>{primary.label}</Button>;

  let body: ReactNode;
  if (all === null && !resource.error) {
    body = (
      <div aria-busy="true" aria-label={t('brood.table.loading')}>
        <Table>
          {heads}
          <TableBody>
            {[0, 1, 2, 3].map((line) => (
              <TableRow key={line}>
                {columns.map((column) => (
                  <TableCell key={column.id}>
                    <Skeleton width="70%" height={14} />
                  </TableCell>
                ))}
                {actions && <TableCell />}
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </div>
    );
  } else if (all === null) {
    body = (
      <div className={styles.loadFailure} role="alert">
        <p>{t('brood.table.load_failed', { error: resource.error })}</p>
        <Button variant="secondary" size="sm" onClick={() => void resource.refresh()}>
          {t('brood.table.retry')}
        </Button>
      </div>
    );
  } else if (all.length === 0) {
    body = <EmptyState title={t('brood.table.empty')} description={empty} action={create} />;
  } else {
    body = (
      <>
        <div className={styles.filters} role="search">
          <label className={styles.search}>
            <span className={styles.srOnly}>{t('brood.table.filter')}</span>
            <IconSearch size={15} aria-hidden="true" />
            <input
              className="input"
              type="search"
              value={text}
              placeholder={t('brood.table.filter')}
              autoComplete="off"
              spellCheck={false}
              onChange={(event) => query.set({ q: event.target.value })}
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
              {t('brood.table.clear_filters')}
            </Button>
          )}
          <span className={styles.count} aria-live="polite">
            {filtered
              ? t('brood.table.shown', { shown: rows.length, total: all.length })
              : t('brood.table.total', { count: all.length })}
          </span>
        </div>
        {resource.error && (
          <p className={styles.failure} role="alert">
            {t('brood.table.refresh_failed', { error: resource.error })}
          </p>
        )}
        {rows.length === 0 ? (
          <EmptyState
            title={t('brood.table.no_match')}
            description={t('brood.table.no_match_hint')}
            action={
              <Button variant="secondary" size="sm" onClick={clear}>
                {t('brood.table.clear_filters')}
              </Button>
            }
          />
        ) : (
          <Table className={styles.table}>
            {heads}
            <TableBody>
              {rows.map((row) => (
                <TableRow key={rowKey(row)}>
                  {columns.map((column) => (
                    <TableCell key={column.id} alignRight={column.numeric}>
                      {column.cell(row)}
                    </TableCell>
                  ))}
                  {actions && (
                    <TableCell className={styles.actionsCell}>{rowActions(row)}</TableCell>
                  )}
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </>
    );
  }

  return (
    <div className={styles.page}>
      <PageHeader title={title} description={description} actions={create} />
      {body}
      {children}
      <ConfirmDialog request={confirmation} close={() => setConfirmation(null)} />
    </div>
  );
}
