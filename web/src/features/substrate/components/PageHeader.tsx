import { Fragment, type ReactNode } from 'react';
import styles from '../substrate.module.scss';

interface PageHeaderProps {
  title: ReactNode;
  /** Items of the mono meta line, joined by dots; falsy ones are skipped. */
  meta?: ReactNode[];
  actions?: ReactNode;
  headingLevel?: 1 | 2;
}

/** brood: page header in the upstream pages' language -- large title, mono
 * meta line with the ▍ cursor, actions aligned to the bottom of the title so
 * they never sit under the floating header bar. */
export function PageHeader({ title, meta = [], actions, headingLevel = 1 }: PageHeaderProps) {
  const Heading = headingLevel === 1 ? 'h1' : 'h2';
  const items = meta.filter(Boolean);
  return (
    <header className={styles.header}>
      <div className={styles.copy}>
        <Heading className={styles.title}>{title}</Heading>
        {items.length > 0 ? (
          <p className={styles.meta}>
            {items.map((item, index) => (
              <Fragment key={index}>
                {index > 0 && (
                  <span className={styles.metaDot} aria-hidden="true">
                    ·
                  </span>
                )}
                <span>{item}</span>
              </Fragment>
            ))}
          </p>
        ) : (
          /* Без meta колонка заголовка ниже кнопок действий, и те, прижатые
           * к её низу, вылезали вверх под плавающую шапку (fixed, 80px при
           * отступе контента 70px). Пустая строка meta держит высоту (#256). */
          <p className={styles.meta} aria-hidden="true" style={{ visibility: 'hidden' }} />
        )}
      </div>
      {actions && <div className={styles.actions}>{actions}</div>}
    </header>
  );
}
