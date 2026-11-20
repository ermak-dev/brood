import { useEffect, useId, useLayoutEffect, useRef, useState, type CSSProperties } from 'react';
import { createPortal } from 'react-dom';
import { IconMoreHorizontal } from '@/components/ui/icons';
import styles from './table.module.scss';

export interface MenuItem {
  id: string;
  label: string;
  danger?: boolean;
  /** Why the item cannot be used now; it stays focusable to say so. */
  disabled?: string;
  select: () => void;
}

const GAP = 4;
const MARGIN = 8;

/** Overflow menu of a row. It is rendered in the body, so the table's
 * scroll box does not clip it, and it lives in its own state: a refresh of
 * the rows leaves it open. Focus returns to the trigger before an item
 * acts, so a dialog the item opens gives focus back to the trigger. */
export function RowMenu({ label, items }: { label: string; items: MenuItem[] }) {
  const id = useId();
  const [open, setOpen] = useState(false);
  const [style, setStyle] = useState<CSSProperties | null>(null);
  const trigger = useRef<HTMLButtonElement | null>(null);
  const menu = useRef<HTMLDivElement | null>(null);

  const close = (focus = true) => {
    setOpen(false);
    if (focus) trigger.current?.focus({ preventScroll: true });
  };

  useLayoutEffect(() => {
    if (!open || !trigger.current || !menu.current) return;
    const anchor = trigger.current.getBoundingClientRect();
    const size = menu.current.getBoundingClientRect();
    const below = anchor.bottom + GAP + size.height <= window.innerHeight - MARGIN;
    setStyle({
      top: below ? anchor.bottom + GAP : Math.max(MARGIN, anchor.top - GAP - size.height),
      left: Math.max(MARGIN, anchor.right - size.width),
    });
    menu.current.querySelector<HTMLElement>('[role="menuitem"]')?.focus({ preventScroll: true });
  }, [open]);

  useEffect(() => {
    if (!open) return;
    const outside = (event: MouseEvent) => {
      const target = event.target as Node;
      if (!menu.current?.contains(target) && !trigger.current?.contains(target)) setOpen(false);
    };
    const away = (event: Event) => {
      if (!menu.current?.contains(event.target as Node)) setOpen(false);
    };
    document.addEventListener('mousedown', outside);
    window.addEventListener('resize', away);
    window.addEventListener('scroll', away, true);
    return () => {
      document.removeEventListener('mousedown', outside);
      window.removeEventListener('resize', away);
      window.removeEventListener('scroll', away, true);
    };
  }, [open]);

  const onKeyDown = (event: React.KeyboardEvent<HTMLDivElement>) => {
    const entries = Array.from(
      menu.current?.querySelectorAll<HTMLElement>('[role="menuitem"]') ?? []
    );
    const index = entries.indexOf(document.activeElement as HTMLElement);
    const focus = (next: number) =>
      entries[(next + entries.length) % entries.length]?.focus({ preventScroll: true });
    switch (event.key) {
      case 'ArrowDown':
        focus(index + 1);
        break;
      case 'ArrowUp':
        focus(index - 1);
        break;
      case 'Home':
        focus(0);
        break;
      case 'End':
        focus(entries.length - 1);
        break;
      case 'Escape':
        event.stopPropagation();
        close();
        break;
      case 'Tab':
        close();
        break;
      default:
        return;
    }
    event.preventDefault();
  };

  if (items.length === 0) return null;
  return (
    <>
      <button
        ref={trigger}
        type="button"
        className={`btn btn-ghost btn-sm ${styles.menuTrigger}`}
        aria-label={label}
        title={label}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls={open ? id : undefined}
        onClick={() => setOpen(!open)}
        onKeyDown={(event) => {
          if (event.key === 'ArrowDown' && !open) {
            event.preventDefault();
            setOpen(true);
          }
        }}
      >
        <IconMoreHorizontal size={16} />
      </button>
      {open &&
        createPortal(
          <div
            ref={menu}
            id={id}
            role="menu"
            aria-label={label}
            className={styles.menu}
            // Off screen until measured; hidden would refuse the focus.
            style={style ?? { top: -9999, left: -9999 }}
            onKeyDown={onKeyDown}
          >
            {items.map((item) => (
              <button
                key={item.id}
                type="button"
                role="menuitem"
                tabIndex={-1}
                className={`${styles.menuItem} ${item.danger ? styles.menuItemDanger : ''}`}
                aria-disabled={item.disabled ? true : undefined}
                title={item.disabled}
                onClick={() => {
                  if (item.disabled) return;
                  close();
                  item.select();
                }}
              >
                {item.label}
              </button>
            ))}
          </div>,
          document.body
        )}
    </>
  );
}
