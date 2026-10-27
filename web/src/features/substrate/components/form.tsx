import { useId, type ReactNode } from 'react';
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

interface SectionProps {
  title: string;
  hint?: ReactNode;
  action?: ReactNode;
  children: ReactNode;
}

/** brood: a titled group of fields inside an editor. */
export function Section({ title, hint, action, children }: SectionProps) {
  return (
    <section className={styles.section}>
      <div className={styles.sectionHead}>
        <div>
          <h3 className={styles.sectionTitle}>{title}</h3>
          {hint && <p className={styles.sectionHint}>{hint}</p>}
        </div>
        {action}
      </div>
      {children}
    </section>
  );
}
