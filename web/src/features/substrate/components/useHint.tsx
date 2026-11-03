import { useEffect, useId, useRef, useState, type FocusEvent, type MouseEvent } from 'react';
import { createPortal } from 'react-dom';
import { HintBubble } from './HintBubble';
import { useEscapeLayer } from './floating';

const SHOW_DELAY = 350;
const HIDE_DELAY = 120;

const focusVisible = (element: HTMLElement) => {
  try {
    return element.matches(':focus-visible');
  } catch {
    return true;
  }
};

/** brood: видимая подсказка элемента — по наведению (с задержкой) и по фокусу
 * с клавиатуры; тот же текст — его accessible description, поэтому причина
 * недоступности действия слышна и видна, а не только серый цвет. Escape
 * закрывает подсказку, курсор можно перевести на неё (WCAG 1.4.13). */
export function useHint(text: string | undefined) {
  const id = useId();
  const [rect, setRect] = useState<DOMRect | null>(null);
  const timer = useRef<number | undefined>(undefined);
  const clear = () => {
    if (timer.current !== undefined) window.clearTimeout(timer.current);
    timer.current = undefined;
  };
  const later = (action: () => void, delay: number) => {
    clear();
    timer.current = window.setTimeout(action, delay);
  };
  const hide = () => {
    clear();
    setRect(null);
  };
  useEscapeLayer(rect !== null, hide);
  useEffect(() => clear, []);

  if (!text) return { trigger: {}, node: null };
  return {
    trigger: {
      'aria-describedby': id,
      onMouseEnter: (event: MouseEvent<HTMLElement>) => {
        const element = event.currentTarget;
        later(() => setRect(element.getBoundingClientRect()), SHOW_DELAY);
      },
      onMouseLeave: () => later(() => setRect(null), HIDE_DELAY),
      onFocus: (event: FocusEvent<HTMLElement>) => {
        if (focusVisible(event.currentTarget)) setRect(event.currentTarget.getBoundingClientRect());
      },
      onBlur: hide,
    },
    node: (
      <>
        <span id={id} hidden>
          {text}
        </span>
        {rect &&
          typeof document !== 'undefined' &&
          createPortal(
            <HintBubble rect={rect} text={text} onEnter={clear} onLeave={() => later(hide, HIDE_DELAY)} />,
            document.body
          )}
      </>
    ),
  };
}
