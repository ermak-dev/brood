// FEATURE: Состояния загрузки доступа и защита от обновления прежнего проекта.
// SPEC: Ошибка сервера показана текстом с повтором; старый callback не грузит прежний контекст.
// IMPLEMENTATION: Общий LoadState и отсечение callback после cleanup.
// RESULT: IMPLEMENTED — SSR и регрессия старого refresh проходят.
// STATUS: DONE
import { expect, spyOn, test } from 'bun:test';
import * as React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import * as header from '@/hooks/useHeaderRefresh';
import { LoadState, useAccessResource } from '@/features/access/shared';

test('renders server refusals accessibly and hides old table data', () => {
  const markup = renderToStaticMarkup(
    React.createElement(LoadState, {
      loading: false,
      error: 'membership revoked <reason>',
      retry: async () => {},
      children: 'stale swarm table',
    })
  );
  expect(markup).toContain('role="alert"');
  expect(markup).toContain('membership revoked &lt;reason&gt;');
  expect(markup).toContain('<button');
  expect(markup).not.toContain('stale swarm table');
});

test('an old mutation refresh cannot reload a previous project after scope cleanup', async () => {
  const cleanups: Array<() => void> = [];
  const calls: string[] = [];
  const state = spyOn(React, 'useState').mockImplementation(((value: unknown) => [
    value,
    () => {},
  ]) as typeof React.useState);
  const ref = spyOn(React, 'useRef').mockImplementation(((value: unknown) => ({
    current: value,
  })) as typeof React.useRef);
  const callback = spyOn(React, 'useCallback').mockImplementation((value) => value);
  const effect = spyOn(React, 'useEffect').mockImplementation((action) => {
    const cleanup = action();
    if (cleanup) cleanups.push(cleanup);
  });
  const register = spyOn(header, 'useHeaderRefresh').mockImplementation(() => {});
  try {
    const resource = useAccessResource(async () => {
      calls.push('old project');
      return [];
    });
    expect(calls).toEqual(['old project']);
    cleanups.forEach((cleanup) => cleanup());
    await resource.refresh();
    expect(calls).toEqual(['old project']);
  } finally {
    state.mockRestore();
    ref.mockRestore();
    callback.mockRestore();
    effect.mockRestore();
    register.mockRestore();
  }
});
