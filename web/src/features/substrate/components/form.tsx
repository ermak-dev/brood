import { createContext, useContext, useId, type ReactNode } from 'react';
import styles from '../substrate.module.scss';

interface FieldProps {
  label: string;
  hint?: ReactNode;
  error?: string;
  /** Receives the id to put on the control, so the label points at it. */
  children: (id: string) => ReactNode;
}

/** brood: a labelled form control with its hint or error under it. */
export function Field({ label, hint, error, children }: FieldProps) {
  const id = useId();
  return (
    <div className={styles.field}>
      <label className={styles.fieldLabel} htmlFor={id}>
        {label}
      </label>
      {children(id)}
      {error ? (
        <p className={styles.fieldError} role="alert">
          {error}
        </p>
      ) : (
        hint && <p className={styles.fieldHint}>{hint}</p>
      )}
    </div>
  );
}

type HeadingLevel = 2 | 3 | 4 | 5 | 6;

/** Уровень заголовка секции по вложенности (#256): страница даёт h1
 * (PageHeader), секция в ней — h2, секция внутри секции — h3. Раньше
 * каждая секция была h3, и порядок заголовков шёл h1 → h3. */
const SectionLevel = createContext<HeadingLevel>(2);

interface SectionProps {
  title: string;
  hint?: ReactNode;
  action?: ReactNode;
  /** Явный уровень; без него — по вложенности. */
  headingLevel?: HeadingLevel;
  children: ReactNode;
}

/** brood: a titled group of fields inside an editor. */
export function Section({ title, hint, action, headingLevel, children }: SectionProps) {
  const inherited = useContext(SectionLevel);
  const level = headingLevel ?? inherited;
  const Heading = `h${level}` as 'h2' | 'h3' | 'h4' | 'h5' | 'h6';
  const nested = Math.min(level + 1, 6) as HeadingLevel;
  return (
    <SectionLevel.Provider value={nested}>
      <section className={styles.section}>
        <div className={styles.sectionHead}>
          <div>
            <Heading className={styles.sectionTitle}>{title}</Heading>
            {hint && <p className={styles.sectionHint}>{hint}</p>}
          </div>
          {action}
        </div>
        {children}
      </section>
    </SectionLevel.Provider>
  );
}
