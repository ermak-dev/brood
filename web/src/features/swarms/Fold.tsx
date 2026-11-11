import type { ReactNode } from 'react';
import styles from '@/features/substrate/substrate.module.scss';
import local from './SwarmPage.module.scss';

/** Свёртываемая секция страницы роя (#253): заголовок секции — в summary,
 * рядом с ним короткий итог («3 ready», «2 answered»), тело раскрывается
 * нативным details. open задаёт страница по состоянию (роли ждут человека,
 * история запросов свёрнута); нажатие оператора React не перебивает, пока
 * состояние не изменится. Заголовок остаётся заголовком: читалка и
 * оглавление страницы видят секцию, как любую другую; уровень по
 * умолчанию h2 — как у секции страницы после #256. */
export function Fold({
  title,
  summary,
  hint,
  open,
  headingLevel = 2,
  children,
  ...attributes
}: {
  title: string;
  summary?: ReactNode;
  hint?: ReactNode;
  open: boolean;
  headingLevel?: 2 | 3 | 4;
  children: ReactNode;
  [data: `data-${string}`]: string | undefined;
}) {
  const Heading = `h${headingLevel}` as 'h2' | 'h3' | 'h4';
  return (
    <section className={`${styles.section} ${local.fold}`} {...attributes}>
      <details open={open} className={local.foldDetails}>
        <summary className={local.foldSummary}>
          <Heading className={styles.sectionTitle}>{title}</Heading>
          {summary && <span className={local.foldMeta}>{summary}</span>}
        </summary>
        <div className={local.foldBody}>
          {hint && <p className={styles.sectionHint}>{hint}</p>}
          {children}
        </div>
      </details>
    </section>
  );
}
