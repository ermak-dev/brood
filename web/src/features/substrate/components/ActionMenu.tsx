import {
  useEffect,
  useId,
  useLayoutEffect,
  useRef,
  useState,
  type CSSProperties,
  type KeyboardEvent,
} from 'react';
import { createPortal } from 'react-dom';
import { Button } from '@/components/ui/Button';
import { IconChevronDown } from '@/components/ui/icons';
import { floatingStyle, FLOATING_Z, useEscapeLayer } from './floating';
import styles from '../substrate.module.scss';

export interface MenuAction {
  id: string;
  label: string;
  /** Что делает действие, простыми словами; всегда видно в меню. */
  description: string;
  /** Почему действие сейчас недоступно; есть — пункт заблокирован. */
  blocked?: string;
  danger?: boolean;
  onSelect: () => void;
}

export interface MenuGroup {
  id: string;
  label: string;
  actions: MenuAction[];
}

const MENU_WIDTH = 340;

/** brood: кнопка меню действий (паттерн WAI-ARIA menu button). Пункты —
 * группами с подписью, опасная группа отделена и окрашена. Недоступный пункт
 * остаётся в фокусе стрелками и называет причину словами (aria-disabled, а не
 * disabled). Меню — в портале: таблица прокручивается и обрезала бы его. */
export function ActionMenu({
  label,
  accessibleLabel,
  groups,
  disabled = false,
}: {
  label: string;
  accessibleLabel: string;
  groups: MenuGroup[];
  disabled?: boolean;
}) {
  const menuId = useId();
  const [open, setOpen] = useState(false);
  const [rect, setRect] = useState<DOMRect | null>(null);
  const [style, setStyle] = useState<CSSProperties>({
    position: 'fixed',
    top: 0,
    left: 0,
    visibility: 'hidden',
    zIndex: FLOATING_Z,
  });
  const triggerRef = useRef<HTMLDivElement | null>(null);
  const menuRef = useRef<HTMLDivElement | null>(null);

  const items = () =>
    Array.from(menuRef.current?.querySelectorAll<HTMLElement>('[role="menuitem"]') ?? []);
  const close = (focusTrigger: boolean) => {
    setOpen(false);
    if (focusTrigger) triggerRef.current?.querySelector('button')?.focus();
  };
  const show = () => {
    const button = triggerRef.current?.querySelector('button');
    if (!button) return;
    setRect(button.getBoundingClientRect());
    setOpen(true);
  };

  useEscapeLayer(open, () => close(true));

  useLayoutEffect(() => {
    const menu = menuRef.current;
    if (!open || !rect || !menu) return;
    setStyle(floatingStyle(rect, { width: MENU_WIDTH, height: menu.scrollHeight }));
  }, [open, rect]);

  // visibility:hidden не принимает фокус: ждём commit рассчитанного положения.
  useLayoutEffect(() => {
    if (open && style.visibility !== 'hidden') items()[0]?.focus();
  }, [open, style]);

  // Клик мимо, прокрутка страницы и смена размера закрывают меню: позиция
  // fixed иначе отстала бы от строки.
  useEffect(() => {
    if (!open) return;
    const outside = (event: Event) => {
      const target = event.target as Node;
      if (menuRef.current?.contains(target) || triggerRef.current?.contains(target)) return;
      setOpen(false);
    };
    const dismiss = () => setOpen(false);
    document.addEventListener('mousedown', outside);
    document.addEventListener('scroll', outside, true);
    window.addEventListener('resize', dismiss);
    return () => {
      document.removeEventListener('mousedown', outside);
      document.removeEventListener('scroll', outside, true);
      window.removeEventListener('resize', dismiss);
    };
  }, [open]);

  const onTriggerKey = (event: KeyboardEvent) => {
    if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
      event.preventDefault();
      show();
    }
  };

  const onMenuKey = (event: KeyboardEvent) => {
    const list = items();
    const index = list.indexOf(document.activeElement as HTMLElement);
    const move = (next: number) => {
      event.preventDefault();
      list[(next + list.length) % list.length]?.focus();
    };
    if (event.key === 'ArrowDown') move(index + 1);
    else if (event.key === 'ArrowUp') move(index - 1);
    else if (event.key === 'Home') move(0);
    else if (event.key === 'End') move(list.length - 1);
    else if (event.key === 'Tab') {
      // Фокус возвращается на кнопку меню; ловушка фокуса Sheet (keydown на
      // document) не должна обработать этот же Tab ещё раз.
      event.preventDefault();
      event.stopPropagation();
      close(true);
    }
  };

  const select = (action: MenuAction) => {
    if (action.blocked) return;
    close(true);
    action.onSelect();
  };

  return (
    <div ref={triggerRef} className={styles.menuTrigger}>
      <Button
        type="button"
        variant="secondary"
        size="sm"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls={open ? menuId : undefined}
        aria-label={accessibleLabel}
        disabled={disabled}
        onClick={() => (open ? close(false) : show())}
        onKeyDown={onTriggerKey}
      >
        {label}
        <IconChevronDown size={14} aria-hidden="true" />
      </Button>
      {open &&
        typeof document !== 'undefined' &&
        createPortal(
          <div
            ref={menuRef}
            id={menuId}
            role="menu"
            aria-label={accessibleLabel}
            className={styles.menu}
            style={{ ...style, width: MENU_WIDTH }}
            onKeyDown={onMenuKey}
          >
            {groups.map((group, index) => (
              <MenuSection
                key={group.id}
                group={group}
                separated={index > 0}
                onSelect={select}
              />
            ))}
          </div>,
          document.body
        )}
    </div>
  );
}

function MenuSection({
  group,
  separated,
  onSelect,
}: {
  group: MenuGroup;
  separated: boolean;
  onSelect: (action: MenuAction) => void;
}) {
  const labelId = useId();
  return (
    <>
      {separated && <div role="separator" className={styles.menuSeparator} />}
      <div role="group" aria-labelledby={labelId}>
        <div id={labelId} className={styles.menuGroupLabel}>
          {group.label}
        </div>
        {group.actions.map((action) => (
          <button
            key={action.id}
            type="button"
            role="menuitem"
            tabIndex={-1}
            aria-disabled={action.blocked ? true : undefined}
            aria-describedby={`${labelId}-${action.id}`}
            className={[
              styles.menuItem,
              action.danger ? styles.menuItemDanger : '',
              action.blocked ? styles.menuItemBlocked : '',
            ]
              .filter(Boolean)
              .join(' ')}
            onClick={() => onSelect(action)}
          >
            <span className={styles.menuItemLabel}>{action.label}</span>
            <span id={`${labelId}-${action.id}`} className={styles.menuItemText}>
              {action.description}
              {action.blocked && (
                <span className={styles.menuItemReason}>{action.blocked}</span>
              )}
            </span>
          </button>
        ))}
      </div>
    </>
  );
}
