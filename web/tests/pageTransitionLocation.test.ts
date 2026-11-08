// BUG: Без ключа совпадающие адреса смешивают записи; state-only изменения теряются.
// HYPOTHESIS: Идентичность выводится из URL, а state не участвует в сравнении.
// SOLUTION: Использовать только известный ключ записи и учитывать state.
// RESULT: FIXED — неизвестные записи не сопоставляются, state доходит до слоя.
// STATUS: FIXED
import { describe, expect, test } from 'bun:test';
import type { Location } from 'react-router-dom';
import {
  classifyLocationChange,
  createLayerKey,
  findLayerIndex,
  updateCurrentLayerLocation,
  type PageLayer,
} from '@/components/common/pageTransitionLocation';

// Hash history gives every entry created outside the router (address bar,
// location.hash, DevTools) the key "default", so it is the default here.
const at = (path: string, key = 'default'): Location => {
  const url = new URL(path, 'http://panel.test');
  return { pathname: url.pathname, search: url.search, hash: url.hash, state: null, key };
};

const layer = (key: string, location: Location, status: PageLayer['status']): PageLayer => ({
  key,
  location,
  status,
});

describe('page transition location change', () => {
  test('a direct hash change to another page changes the page', () => {
    expect(classifyLocationChange(at('/actors'), at('/swarms'))).toBe('navigate');
  });

  test('a different pathname changes the page whatever the keys are', () => {
    expect(classifyLocationChange(at('/actors', 'k1'), at('/swarms', 'k2'))).toBe('navigate');
    expect(classifyLocationChange(at('/actors', 'k1'), at('/swarms'))).toBe('navigate');
    expect(classifyLocationChange(at('/actors'), at('/swarms', 'k1'))).toBe('navigate');
  });

  test('a query change on the same page updates the page in place', () => {
    expect(classifyLocationChange(at('/actors'), at('/actors?swarm=x'))).toBe('update');
    expect(classifyLocationChange(at('/actors', 'k1'), at('/actors?swarm=x', 'k2'))).toBe('update');
    expect(classifyLocationChange(at('/actors?swarm=x'), at('/actors?swarm=y'))).toBe('update');
  });

  test('a hash change on the same page updates the page in place', () => {
    expect(classifyLocationChange(at('/config'), at('/config#routing'))).toBe('update');
  });

  test('a new history entry for the same address updates the page in place', () => {
    expect(classifyLocationChange(at('/actors', 'k1'), at('/actors', 'k2'))).toBe('update');
  });

  test('the identical location changes nothing', () => {
    expect(classifyLocationChange(at('/actors?swarm=x', 'k1'), at('/actors?swarm=x', 'k1'))).toBe(
      'none'
    );
    expect(classifyLocationChange(at('/actors?swarm=x'), at('/actors?swarm=x'))).toBe('none');
  });

  test('without a current layer the page changes', () => {
    expect(classifyLocationChange(undefined, at('/actors'))).toBe('navigate');
  });
});

describe('page transition history entries', () => {
  // Opened /auth-files directly, then pushed an editor through the router.
  const layers = [
    layer('default', at('/auth-files'), 'stacked'),
    layer('k1', at('/auth-files/oauth-excluded', 'k1'), 'current'),
  ];

  test('a direct entry cannot be identified by its address', () => {
    expect(findLayerIndex(layers, at('/auth-files'))).toBe(-1);
  });

  test('state-only changes update the page', () => {
    expect(classifyLocationChange(at('/actors', 'k1'), { ...at('/actors', 'k1'), state: { x: 1 } })).toBe('update');
  });

  test('going back finds a layer by its router key', () => {
    expect(findLayerIndex(layers, at('/auth-files/oauth-excluded', 'k1'))).toBe(1);
  });

  test('an unrelated direct entry is not mistaken for an existing layer', () => {
    expect(findLayerIndex(layers, at('/auth-files/oauth-model-alias'))).toBe(-1);
  });

  test('a router entry is not mistaken for a layer with another key', () => {
    expect(findLayerIndex(layers, at('/auth-files', 'k2'))).toBe(-1);
  });
});

describe('page transition layer keys', () => {
  test('a free location key is used as is', () => {
    expect(createLayerKey('k1', [])).toBe('k1');
    expect(createLayerKey('default', ['k1'])).toBe('default');
  });

  test('repeated default keys still give every layer its own key', () => {
    const keys: string[] = [];
    for (let index = 0; index < 4; index += 1) {
      keys.push(createLayerKey('default', keys));
    }
    expect(new Set(keys).size).toBe(4);
  });
});

describe('page transition in-place update', () => {
  test('the current layer keeps its key and sees the new location', () => {
    const stacked = layer('a', at('/auth-files', 'a'), 'stacked');
    const current = layer('b', at('/actors', 'b'), 'current');
    const next = at('/actors?swarm=x');

    const updated = updateCurrentLayerLocation([stacked, current], next);

    expect(updated).toEqual([stacked, { key: 'b', location: next, status: 'current' }]);
    expect(current.location.search).toBe('');
  });

  test('without a current status the last layer is the current one', () => {
    const first = layer('a', at('/actors', 'a'), 'stacked');
    const last = layer('b', at('/actors', 'b'), 'stacked');
    const next = at('/actors?swarm=x');

    expect(updateCurrentLayerLocation([first, last], next)).toEqual([
      first,
      { ...last, location: next },
    ]);
  });
});
