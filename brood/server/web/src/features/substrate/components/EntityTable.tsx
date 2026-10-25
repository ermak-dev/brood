import type { KeyboardEvent, ReactNode } from 'react';
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
  actions?: (row: T) => ReactNode;
  actionsLabel?: string;
  onRowClick?: (row: T) => void;
  isActive?: (row: T) => boolean;
}

/** brood: a list of Substrate objects -- a table on a desktop, a stack of cards
 * on a phone (each cell labelled by its column). Row actions stop the click
 * from opening the row. */
export function EntityTable<T>({
  columns,
  rows,
  rowKey,
  actions,
  actionsLabel = '',
  onRowClick,
  isActive,
}: EntityTableProps<T>) {
  const cellClass = (column: Column<T>) =>
    [
      column.primary ? styles.cellPrimary : '',
      column.mono ? styles.mono : '',
      column.hideOnMobile ? styles.hideOnMobile : '',
    ]
      .filter(Boolean)
      .join(' ') || undefined;

  const openOnKey = (event: KeyboardEvent, row: T) => {
    if (onRowClick && (event.key === 'Enter' || event.key === ' ') && event.target === event.currentTarget) {
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
            </tr>
          </thead>
          <tbody>
            {rows.map((row) => (
              <tr
                key={rowKey(row)}
                className={
                  [onRowClick ? styles.rowClickable : '', isActive?.(row) ? styles.rowActive : '']
                    .filter(Boolean)
                    .join(' ') || undefined
                }
                onClick={onRowClick ? () => onRowClick(row) : undefined}
                onKeyDown={(event) => openOnKey(event, row)}
                tabIndex={onRowClick ? 0 : undefined}
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
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}
