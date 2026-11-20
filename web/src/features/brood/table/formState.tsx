import { Fragment, useState, type ChangeEvent, type ReactNode } from 'react';

/** One dialog of a page: every opening is a new session, so the form inside
 * starts clean (and a secret typed into it does not outlive it), while the
 * closed dialog stays mounted and the Modal returns focus to its trigger. */
export function useDialog<P = null>() {
  const [state, setState] = useState<{ open: boolean; session: number; payload: P | null }>({
    open: false,
    session: 0,
    payload: null,
  });
  return {
    show: (payload: P) => setState({ open: true, session: state.session + 1, payload }),
    mount: (render: (dialog: { open: boolean; close: () => void }, payload: P) => ReactNode) =>
      state.session > 0 && (
        <Fragment key={state.session}>
          {render(
            { open: state.open, close: () => setState((prev) => ({ ...prev, open: false })) },
            state.payload as P
          )}
        </Fragment>
      ),
  };
}

type Errors<F extends string> = Partial<Record<F, string>>;

/** Text fields of a form with their validation. A message appears next to
 * its field once the field was left or holds something invalid; `valid`
 * counts every message, shown or not. */
export function useForm<F extends string>(
  initial: Record<F, string>,
  validate: (values: Record<F, string>) => Errors<F>
) {
  const [values, setValues] = useState(initial);
  const [touched, setTouched] = useState<Partial<Record<F, boolean>>>({});
  const errors = validate(values);
  const set = (name: F, value: string) => setValues((prev) => ({ ...prev, [name]: value }));
  return {
    values,
    set,
    valid: Object.values(errors).every((message) => !message),
    /** Props of the shared Input bound to the field. */
    field: (name: F) => ({
      value: values[name],
      error: touched[name] || values[name] !== '' ? errors[name] : undefined,
      onChange: (event: ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) =>
        set(name, event.target.value),
      onBlur: () => setTouched((prev) => ({ ...prev, [name]: true })),
    }),
  };
}
