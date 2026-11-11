// BUG (#262): After the session is restored the panel opens only the path of the address it
// was reloaded at: `LoginPage` navigated to `from.pathname`, so `#/swarms/<id>?probe=1`
// became `#/swarms/<id>`. A sign-in through the form went to the root in any case (#240).
// SOLUTION: The return address is the whole router address (path, query, hash) that
// ProtectedRoute sent to sign-in, and only an internal router path: history state comes
// from the browser, and `//host` or `/\host` is read by a browser as another origin.
// The browser regression is tests/history.browser.ts (address-e2e reload-*, sign-in).
import { describe, expect, test } from 'bun:test';
import { returnAddress } from '@/router/returnAddress';

const from = (location: Record<string, unknown>) => ({ from: location });

describe('return address after sign-in', () => {
  test('keeps the query and hash of the address', () => {
    expect(
      returnAddress(from({ pathname: '/swarms/s-docs', search: '?probe=1', hash: '#step-2' }))
    ).toBe('/swarms/s-docs?probe=1#step-2');
    expect(
      returnAddress(from({ pathname: '/projects/p-brood', search: '?project=p-brood', hash: '' }))
    ).toBe('/projects/p-brood?project=p-brood');
  });

  test('is the path alone when there is no query or hash', () => {
    expect(returnAddress(from({ pathname: '/swarms/s-docs', search: '', hash: '' }))).toBe(
      '/swarms/s-docs'
    );
    expect(returnAddress(from({ pathname: '/swarms' }))).toBe('/swarms');
  });

  test('is the root without an address to return to', () => {
    expect(returnAddress(null)).toBe('/');
    expect(returnAddress(undefined)).toBe('/');
    expect(returnAddress({})).toBe('/');
    expect(returnAddress('/swarms')).toBe('/');
    expect(returnAddress({ from: '/swarms' })).toBe('/');
    expect(returnAddress(from({ search: '?probe=1' }))).toBe('/');
  });

  test('never leaves the router for another origin', () => {
    for (const pathname of [
      'https://evil.example/swarms',
      'evil.example',
      '//evil.example/swarms',
      '/\\evil.example',
      '/\t/evil.example',
      '/\n/evil.example',
      '',
    ]) {
      expect(returnAddress(from({ pathname }))).toBe('/');
    }
  });

  test('refuses parts that would change the address when joined', () => {
    // A query or hash that does not start with its mark would join the path.
    expect(returnAddress(from({ pathname: '/', search: '/evil.example' }))).toBe('/');
    expect(returnAddress(from({ pathname: '/', hash: '/evil.example' }))).toBe('/');
    // A mark inside an earlier part moves the boundary between the parts.
    expect(returnAddress(from({ pathname: '/swarms?probe=1' }))).toBe('/');
    expect(returnAddress(from({ pathname: '/swarms#step' }))).toBe('/');
    expect(returnAddress(from({ pathname: '/swarms', search: '?a#b' }))).toBe('/');
    expect(returnAddress(from({ pathname: '/swarms', search: 1 }))).toBe('/');
  });
});
