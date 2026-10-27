import { expect, spyOn, test } from 'bun:test';
import * as React from 'react';
import * as header from '@/hooks/useHeaderRefresh';
import { useResource } from '@/features/substrate/hooks';

// FEATURE: Local editors do not claim the page's header refresh handler.
// SPEC: A resource can opt out, while list pages register by default.
// IMPLEMENTATION: Editors disable header registration on the shared resource hook.
// RESULT: IMPLEMENTED — local loads no longer replace the page refresh handler.
// STATUS: DONE
test('editor resources leave the page header refresh registration alone', () => {
  const state = spyOn(React, 'useState').mockImplementation(((value: unknown) => [value, () => {}]) as typeof React.useState);
  const ref = spyOn(React, 'useRef').mockImplementation(((value: unknown) => ({ current: value })) as typeof React.useRef);
  const callback = spyOn(React, 'useCallback').mockImplementation((value) => value);
  const effect = spyOn(React, 'useEffect').mockImplementation(() => {});
  const register = spyOn(header, 'useHeaderRefresh').mockImplementation(() => {});
  try {
    const load = async () => [];
    const local = useResource(load, { headerRefresh: false });
    expect(register).toHaveBeenLastCalledWith(local.refresh, false);
    const page = useResource(load);
    expect(register).toHaveBeenLastCalledWith(page.refresh, true);
  } finally {
    state.mockRestore(); ref.mockRestore(); callback.mockRestore();
    effect.mockRestore(); register.mockRestore();
  }
});
