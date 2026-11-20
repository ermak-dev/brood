import { Fragment, useState, type ChangeEvent, type ReactNode } from 'react';

/** One sheet of a page: every opening is a new session, so the form inside
 * starts clean (and a secret typed into it does not outlive it), while the
 * closed sheet stays mounted and returns focus to its trigger. */
export function useDialog<P = null>() {
  const [state, setState] = useState<{ open: boolean; session: number; payload: P | null }>({
    open: false,
    session: 0,
    payload: null,
  });
  return {
    show: (payload: P) =>
      setState((prev) => ({ open: true, session: prev.session + 1, payload })),
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

/** Text fields of a form with their validation. A field is checked when it
 * is left and all of them on submit (`check`); the message appears next to
 * the field. `dirty` says the user changed something worth asking about
 * before the sheet closes. */
export function useForm<F extends string>(
  initial: Record<F, string>,
  validate: (values: Record<F, string>) => Errors<F>
) {
  const [start] = useState(initial);
  const [values, setValues] = useState(initial);
  const [touched, setTouched] = useState<Partial<Record<F, boolean>>>({});
  const [submitted, setSubmitted] = useState(false);
  const errors = validate(values);
  const set = (name: F, value: string) => setValues((prev) => ({ ...prev, [name]: value }));
  const valid = Object.values(errors).every((message) => !message);
  return {
    values,
    set,
    dirty: (Object.keys(values) as F[]).some((name) => values[name] !== start[name]),
    /** Shows every message; false when something is wrong. */
    check: () => {
      setSubmitted(true);
      return valid;
    },
    error: (name: F) => (submitted || touched[name] ? errors[name] : undefined),
    /** Props of the shared Input bound to the field. */
    field: (name: F) => ({
      value: values[name],
      error: submitted || touched[name] ? errors[name] : undefined,
      onChange: (event: ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) =>
        set(name, event.target.value),
      onBlur: () => setTouched((prev) => ({ ...prev, [name]: true })),
    }),
  };
}
