// FEATURE: Locale keys of the run graph and the menu after the Workflows
// section is gone (#217, epic #204).
// SPEC: The `pipelines` namespace keeps only what the panel still names: the
// graph label, the empty graph text and the node kinds. Every key the source
// names, literally or as a `prefix.${value}` family, exists in all six
// locales with the interpolations of English; no locale keeps a `pipelines`,
// `nav` or `nav_meta` key that nothing names.
import { describe, expect, test } from 'bun:test';
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

const sourceRoot = fileURLToPath(new URL('../src/', import.meta.url));
const locales = ['en', 'ru', 'zh-CN', 'zh-TW', 'ko', 'vi'];
const NAMESPACES = ['pipelines', 'nav', 'nav_meta'];
// Подписи меню, осиротевшие до #217 (#193); их удаление — не эта задача.
const EARLIER_ORPHANS = [
  'nav.dashboard',
  'nav.policy',
  'nav.projects',
  'nav_meta.policy',
  'nav_meta.quick_start',
];

const files = (dir: string): string[] =>
  readdirSync(dir).flatMap((name) => {
    const path = join(dir, name);
    return statSync(path).isDirectory() ? files(path) : [path];
  });
const sources = files(sourceRoot)
  .filter((path) => /\.(ts|tsx)$/.test(path))
  .map((path) => readFileSync(path, 'utf8'))
  .join('\n');

const flatten = (value: unknown, prefix = ''): Record<string, string> => {
  if (!value || typeof value !== 'object' || Array.isArray(value)) {
    return prefix ? { [prefix]: String(value ?? '') } : {};
  }
  return Object.entries(value).reduce<Record<string, string>>((leaves, [key, child]) => {
    const path = prefix ? `${prefix}.${key}` : key;
    return { ...leaves, ...flatten(child, path) };
  }, {});
};
const read = (locale: string) =>
  flatten(
    JSON.parse(readFileSync(join(sourceRoot, 'i18n', 'locales', `${locale}.json`), 'utf8'))
  );
const tokens = (value: string) => (value.match(/\{\{[^}]+\}\}/g) ?? []).sort().join('|');

const namespace = NAMESPACES.join('|');
// Ключ, названный строкой, и семейство `prefix.${value}`.
const literal = new Set(
  [...sources.matchAll(new RegExp(`['"\`]((?:${namespace})\\.[a-z0-9_.]+)['"\`]`, 'g'))].map(
    (match) => match[1]
  )
);
const families = [
  ...new Set(
    [...sources.matchAll(new RegExp(`\`((?:${namespace})\\.[a-z0-9_.]*)\\$\\{`, 'g'))].map(
      (match) => match[1]
    )
  ),
];
const owned = (key: string) => NAMESPACES.some((name) => key.startsWith(`${name}.`));
const named = (key: string) =>
  literal.has(key) || families.some((family) => key.startsWith(family));

const english = read('en');

describe('graph and menu locale keys', () => {
  test('the source names the graph keys this check is about', () => {
    expect([...literal]).toContain('pipelines.graph_empty');
    expect([...literal]).toContain('pipelines.graph_label');
    expect(families).toContain('pipelines.kind.');
  });

  test('every named key exists in every locale with the English interpolations', () => {
    for (const locale of locales) {
      const translated = read(locale);
      const missing = [...literal].filter((key) => !translated[key]?.trim());
      const emptyFamilies = families.filter(
        (family) => !Object.keys(translated).some((key) => key.startsWith(family))
      );
      expect({ locale, missing, emptyFamilies }).toEqual({
        locale,
        missing: [],
        emptyFamilies: [],
      });
      const keys = Object.keys(english).filter((key) => owned(key) && named(key));
      const mismatched = keys.filter(
        (key) => !(key in translated) || tokens(translated[key]) !== tokens(english[key])
      );
      expect({ locale, mismatched }).toEqual({ locale, mismatched: [] });
    }
  });

  test('no locale keeps a key nothing names', () => {
    for (const locale of locales) {
      const orphans = Object.keys(read(locale)).filter(
        (key) => owned(key) && !named(key) && !EARLIER_ORPHANS.includes(key)
      );
      expect({ locale, orphans }).toEqual({ locale, orphans: [] });
    }
  });

  // BUG: узлы parallel и fan_in подписывались сырым ключом в таймлайне
  // («pipelines.kind.parallel») и сырым видом в имени кнопки графа.
  // Список — типы узлов парсера (go/brood/pipeline/pipeline.go) и start
  // из piprender; каталог go/ в контейнер сборки панели не монтируется.
  test('every engine node kind has a label in every locale', () => {
    const kinds = ['start', 'actor', 'human', 'condition', 'land', 'exit', 'parallel', 'fan_in'];
    for (const locale of locales) {
      const translated = read(locale);
      const missing = kinds.filter((kind) => !translated[`pipelines.kind.${kind}`]?.trim());
      expect({ locale, missing }).toEqual({ locale, missing: [] });
    }
  });
});
