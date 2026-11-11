// FEATURE: Design tokens of the panel (#254, epic #251).
// SPEC: Every text/background token pair of the three themes meets WCAG AA
// (4.5:1) when computed from themes.scss itself: text-tertiary on the page
// and the card, badge foregrounds on badge backgrounds, text on both card
// levels. The type scale is a closed set of --fs-*/--lh-* pairs with one
// line height per size, and the card levels differ in color, not only by a
// border. Lighthouse measures the same numbers in the browser; this test
// measures them before the bundle exists.
import { describe, expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import {
  contrastRatio,
  parseColor,
  parseThemeBlocks,
  resolveToken,
  themeTokens,
  tokenContrast,
} from './helpers/themeTokens';

const scss = readFileSync(
  fileURLToPath(new URL('../src/styles/themes.scss', import.meta.url)),
  'utf8'
);
const blocks = parseThemeBlocks(scss);
const THEMES = [':root', "[data-theme='white']", "[data-theme='dark']"];
const TONES = ['attention', 'progress', 'problem', 'neutral'];
const AA = 4.5;

const ratio = (theme: string, fg: string, bg: string) =>
  Number(tokenContrast(themeTokens(blocks, theme), fg, bg).toFixed(2));

describe('contrast arithmetic', () => {
  test('parses hex and rgb() colors, refuses what a browser would compute', () => {
    expect(parseColor('#fff')).toEqual({ r: 255, g: 255, b: 255, a: 1 });
    expect(parseColor('#a29c95')).toEqual({ r: 162, g: 156, b: 149, a: 1 });
    expect(parseColor('rgba(198, 87, 70, 0.12)')).toEqual({ r: 198, g: 87, b: 70, a: 0.12 });
    expect(parseColor('color-mix(in srgb, #fff 50%, #000)')).toBeNull();
  });

  test('WCAG ratio: black on white is 21, equal colors are 1', () => {
    const black = parseColor('#000')!;
    const white = parseColor('#fff')!;
    expect(contrastRatio(black, white)).toBeCloseTo(21, 1);
    expect(contrastRatio(white, black)).toBeCloseTo(21, 1);
    expect(contrastRatio(white, white)).toBe(1);
  });

  test('var() references resolve through the theme and fall back to :root', () => {
    const dark = themeTokens(blocks, "[data-theme='dark']");
    expect(resolveToken(dark, '--text-muted')).toBe(resolveToken(dark, '--text-tertiary'));
    expect(resolveToken(dark, '--ease-out-strong')).toBe('cubic-bezier(0.23, 1, 0.32, 1)');
  });
});

describe('text on surfaces', () => {
  for (const theme of THEMES) {
    test(`${theme}: primary and secondary text meet AA on page, card and hover`, () => {
      for (const bg of ['--bg-secondary', '--bg-primary', '--bg-tertiary']) {
        expect({ theme, bg, ratio: ratio(theme, '--text-primary', bg) }).toMatchObject({
          ratio: expect.any(Number),
        });
        expect(ratio(theme, '--text-primary', bg)).toBeGreaterThanOrEqual(AA);
        expect(ratio(theme, '--text-secondary', bg)).toBeGreaterThanOrEqual(AA);
      }
    });

    test(`${theme}: tertiary text meets AA on the page and the card`, () => {
      expect(ratio(theme, '--text-tertiary', '--bg-secondary')).toBeGreaterThanOrEqual(AA);
      expect(ratio(theme, '--text-tertiary', '--bg-primary')).toBeGreaterThanOrEqual(AA);
    });

    // Основной цвет красит ссылки на странице и текст кнопки на себе.
    test(`${theme}: the primary color reads as a link and carries button text`, () => {
      expect(ratio(theme, '--primary-color', '--bg-secondary')).toBeGreaterThanOrEqual(AA);
      expect(ratio(theme, '--primary-color', '--bg-primary')).toBeGreaterThanOrEqual(AA);
      expect(ratio(theme, '--primary-contrast', '--primary-color')).toBeGreaterThanOrEqual(AA);
      expect(ratio(theme, '--primary-contrast', '--primary-hover')).toBeGreaterThanOrEqual(AA);
    });
  }
});

describe('badge tones', () => {
  for (const theme of THEMES) {
    test(`${theme}: every tone's foreground meets AA on its background`, () => {
      for (const tone of TONES) {
        const fg = `--${tone}-badge-fg`;
        const bg = `--${tone}-badge-bg`;
        expect({ tone, ratio: ratio(theme, fg, bg) }).toEqual({
          tone,
          ratio: expect.any(Number),
        });
        expect(ratio(theme, fg, bg)).toBeGreaterThanOrEqual(AA);
        // Бейдж лежит на карточке: его рамка должна быть объявлена.
        expect(themeTokens(blocks, theme).has(`--${tone}-badge-border`)).toBe(true);
      }
    });
  }
});

describe('card levels', () => {
  for (const theme of THEMES) {
    test(`${theme}: inset cards differ in color from cards and keep text legible`, () => {
      const tokens = themeTokens(blocks, theme);
      const card = parseColor(resolveToken(tokens, '--card-bg'));
      const inset = parseColor(resolveToken(tokens, '--card-inset-bg'));
      expect(card).not.toBeNull();
      expect(inset).not.toBeNull();
      expect(inset).not.toEqual(card);
      expect(tokens.has('--card-border')).toBe(true);
      expect(tokens.has('--card-inset-border')).toBe(true);
      expect(ratio(theme, '--text-secondary', '--card-bg')).toBeGreaterThanOrEqual(AA);
      expect(ratio(theme, '--text-secondary', '--card-inset-bg')).toBeGreaterThanOrEqual(AA);
      expect(ratio(theme, '--text-tertiary', '--card-inset-bg')).toBeGreaterThanOrEqual(AA);
    });
  }
});

describe('type scale', () => {
  const root = themeTokens(blocks, ':root');
  const px = (name: string) => Number(resolveToken(root, name).replace(/px$/, ''));

  test('sizes 12/13/14/16/20/28 each carry one line height', () => {
    for (const size of [12, 13, 14, 16, 20, 28]) {
      expect(px(`--fs-${size}`)).toBe(size);
      const lh = px(`--lh-${size}`);
      expect(lh).toBeGreaterThanOrEqual(size);
      expect(lh).toBeLessThanOrEqual(size * 1.6);
    }
  });

  test('themes do not override the scale: one scale for all three', () => {
    for (const theme of THEMES.slice(1)) {
      const own = blocks.get(theme)!;
      expect([...own.keys()].filter((name) => /^--(fs|lh)-/.test(name))).toEqual([]);
    }
  });

  test('a measure token keeps card text within 72 characters', () => {
    expect(resolveToken(root, '--measure')).toBe('72ch');
  });
});
