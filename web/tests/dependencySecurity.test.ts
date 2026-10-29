// BUG: #80 — lockfile панели содержит версии с известными advisory.
// HYPOTHESIS: точный pin Axios и устаревшие транзитивные версии удерживают уязвимые пакеты.
// SOLUTION: обновить pin Axios и lockfile до совместимых исправленных версий без новых overrides.
// RESULT: FIXED — security minima проходят, pinned audit не возвращает advisory.
// STATUS: FIXED
import { expect, test } from 'bun:test';

const manifest = await Bun.file(new URL('../package.json', import.meta.url)).json();
const lock = await Bun.file(new URL('../bun.lock', import.meta.url)).text();
const lockedPackages = [...lock.matchAll(/^ {4}"[^"]+": \["([^"]+)"/gm)].map(
  (match) => match[1]
);

// Нижние границы из advisory baseline; проверяем также вложенные копии пакетов.
const securityMinima = [
  ['axios', '1.20.0'],
  ['react-router', '7.18.2'],
  ['react-router-dom', '7.18.2'],
  ['@babel/core', '7.29.6'],
  ['@humanfs/node', '0.16.8'],
  ['baseline-browser-mapping', '2.11.0'],
  ['brace-expansion', '5.0.12'],
  ['browserslist', '4.28.7'],
  ['follow-redirects', '1.16.1'],
  ['immutable', '5.1.8'],
  ['nanoid', '3.3.18'],
  ['postcss', '8.5.23'],
  ['source-map-js', '1.2.2'],
  ['vite', '8.1.6'],
  ['zod', '4.6.6'],
];

test('Axios stays pinned to the verified minimum patched release', () => {
  expect(manifest.dependencies.axios).toBe('1.20.0');
});

test.each(securityMinima)('%s locked copies meet security minimum %s', (name, minimum) => {
  const versions = lockedPackages
    .filter((spec) => spec.startsWith(`${name}@`))
    .map((spec) => spec.slice(name.length + 1));
  expect(versions.length).toBeGreaterThan(0);
  for (const version of versions) {
    expect({ version, patched: Bun.semver.satisfies(version, `>=${minimum}`) }).toEqual({
      version,
      patched: true,
    });
  }
});
