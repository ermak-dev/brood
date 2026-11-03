import type { KeyboardEvent, ReactNode } from 'react';
import { IconChevronRight } from '@/components/ui/icons';
import styles from '../substrate.module.scss';

export interface Column<T> {
  key: string;
  label: string;
  render: (row: T) => ReactNode;
  /** The row's name: bold, and the card title on a phone. */
  primary?: boolean;
  /** Monospace; a long value is cut with an ellipsis, full text on hover. */
  mono?: boolean;
  hideOnMobile?: boolean;
}

interface EntityTableProps<T> {
  columns: Column<T>[];
  rows: T[];
  rowKey: (row: T) => string;
  /** Только для вложенных таблиц внутри панели (участники, права): строки
   * страниц открывают панель объекта (#157), а действия живут в ней. */
  actions?: (row: T) => ReactNode;
  actionsLabel?: string;
  /** Открыть панель объекта; без actions строка получает стрелку справа. */
  onRowClick?: (row: T) => void;
  /** Строка открывается не всегда (golden-актор, native у viewer): такая
   * строка не кликается, не в фокусе и без стрелки. */
  canOpen?: (row: T) => boolean;
  isActive?: (row: T) => boolean;
}

/** brood: a list of Substrate objects -- a table on a desktop, a stack of cards
 * on a phone (each cell labelled by its column). A page row opens the object's
 * side panel (#157); in-panel tables may still carry row actions, which stop
 * the click from opening the row. */
export function EntityTable<T>({
  columns,
  rows,
  rowKey,
  actions,
  actionsLabel = '',
  onRowClick,
  canOpen = () => true,
  isActive,
}: EntityTableProps<T>) {
  const opens = Boolean(onRowClick) && !actions;
  const clickable = (row: T) => Boolean(onRowClick) && canOpen(row);
  const cellClass = (column: Column<T>) =>
    [
      column.primary ? styles.cellPrimary : '',
      column.mono ? styles.mono : '',
      column.hideOnMobile ? styles.hideOnMobile : '',
    ]
      .filter(Boolean)
      .join(' ') || undefined;

  const openOnKey = (event: KeyboardEvent, row: T) => {
    if (onRowClick && clickable(row) && (event.key === 'Enter' || event.key === ' ') && event.target === event.currentTarget) {
      event.preventDefault();
      onRowClick(row);
    }
  };

  return (
    <div className={styles.tableWrap}>
      <div className={styles.tableScroll}>
        <table className={styles.table}>
          <thead>
            <tr>
              {columns.map((column) => (
                <th key={column.key} className={column.hideOnMobile ? styles.hideOnMobile : undefined}>
                  {column.label}
                </th>
              ))}
              {actions && (
                <th className={styles.cellActions}>
                  <span className={styles.srOnly}>{actionsLabel}</span>
                </th>
              )}
              {opens && <th className={styles.cellOpen} aria-hidden="true" />}
            </tr>
          </thead>
          <tbody>
            {rows.map((row) => (
              <tr
                key={rowKey(row)}
                className={
                  [clickable(row) ? styles.rowClickable : '', isActive?.(row) ? styles.rowActive : '']
                    .filter(Boolean)
                    .join(' ') || undefined
                }
                onClick={onRowClick && clickable(row) ? () => onRowClick(row) : undefined}
                onKeyDown={(event) => openOnKey(event, row)}
                tabIndex={clickable(row) ? 0 : undefined}
              >
                {columns.map((column) => {
                  const value = column.render(row);
                  return (
                    <td
                      key={column.key}
                      className={cellClass(column)}
                      data-label={column.label}
                      title={column.mono && typeof value === 'string' ? value : undefined}
                    >
                      {value}
                    </td>
                  );
                })}
                {actions && (
                  <td className={styles.cellActions} onClick={(event) => event.stopPropagation()}>
                    <div className={styles.rowActions}>{actions(row)}</div>
                  </td>
                )}
                {opens && (
                  <td className={styles.cellOpen} aria-hidden="true">
                    {clickable(row) && <IconChevronRight size={16} />}
                  </td>
                )}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}
