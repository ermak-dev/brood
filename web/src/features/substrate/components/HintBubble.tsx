import { useLayoutEffect, useRef, useState, type CSSProperties } from 'react';
import { floatingStyle, FLOATING_Z } from './floating';
import styles from '../substrate.module.scss';

/** Видимая подсказка у якоря; позиция — после измерения собственного размера. */
export function HintBubble({
  rect,
  text,
  onEnter,
  onLeave,
}: {
  rect: DOMRect;
  text: string;
  onEnter: () => void;
  onLeave: () => void;
}) {
  const ref = useRef<HTMLDivElement | null>(null);
  const [style, setStyle] = useState<CSSProperties>({
    position: 'fixed',
    top: 0,
    left: 0,
    visibility: 'hidden',
    zIndex: FLOATING_Z,
  });
  useLayoutEffect(() => {
    const element = ref.current;
    if (!element) return;
    setStyle(
      floatingStyle(rect, { width: element.offsetWidth, height: element.offsetHeight }, 'center')
    );
  }, [rect]);
  return (
    <div
      ref={ref}
      className={styles.hintBubble}
      style={style}
      aria-hidden="true"
      onMouseEnter={onEnter}
      onMouseLeave={onLeave}
    >
      {text}
    </div>
  );
}
