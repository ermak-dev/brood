/**
 * Чистые функции для проверки токенов темы (#254): разбор блоков
 * `themes.scss`, разрешение `var()` и контраст WCAG 2.x. Никакого DOM:
 * тест считает те же числа, что Lighthouse, но из исходника.
 */

export type RGBA = { r: number; g: number; b: number; a: number };

/** Токены одной темы: имя -> необработанное значение. */
export type TokenMap = Map<string, string>;

const BLOCK = /^\s*([^{}\n]+?)\s*\{$/;
const DECLARATION = /^\s*(--[\w-]+)\s*:\s*(.+?)\s*;\s*$/;

/** Блоки верхнего уровня (`:root`, `[data-theme='dark']`, ...) и их
 * объявления custom properties. Вложенные блоки (медиа-запросы) и
 * комментарии пропускаются; блок с тем же селектором дополняет прежний. */
export function parseThemeBlocks(scss: string): Map<string, TokenMap> {
  const themes = new Map<string, TokenMap>();
  let selector: string | null = null;
  let depth = 0;
  for (const raw of scss.split('\n')) {
    const line = raw.replace(/\/\/.*$/, '');
    if (depth === 0) {
      const open = line.match(BLOCK);
      if (open) {
        selector = open[1];
        depth = 1;
        if (!themes.has(selector)) themes.set(selector, new Map());
      }
      continue;
    }
    if (/\{\s*$/.test(line)) {
      depth += 1;
      continue;
    }
    if (/^\s*\}\s*$/.test(line)) {
      depth -= 1;
      if (depth === 0) selector = null;
      continue;
    }
    if (depth !== 1 || selector === null) continue;
    const declaration = line.match(DECLARATION);
    if (declaration) themes.get(selector)!.set(declaration[1], declaration[2]);
  }
  return themes;
}

/** Токены темы с учётом наследования от `:root`: тема переопределяет
 * часть значений, остальные достаются из корня, как в браузере. */
export function themeTokens(blocks: Map<string, TokenMap>, selector: string): TokenMap {
  const merged = new Map(blocks.get(':root') ?? []);
  for (const [name, value] of blocks.get(selector) ?? []) merged.set(name, value);
  return merged;
}

/** Значение токена с раскрытыми ссылками `var(--x)`; ссылка на
 * неизвестный токен остаётся как есть, чтобы отказ назвал её. */
export function resolveToken(tokens: TokenMap, name: string, seen = new Set<string>()): string {
  const raw = tokens.get(name);
  if (raw === undefined || seen.has(name)) return raw ?? `var(${name})`;
  seen.add(name);
  return raw.replace(/var\((--[\w-]+)(?:\s*,\s*[^)]*)?\)/g, (_, ref: string) =>
    resolveToken(tokens, ref, seen)
  );
}

const channel = (hex: string) => parseInt(hex.length === 1 ? hex + hex : hex, 16);

/** Цвет из `#rgb`, `#rrggbb`, `#rrggbbaa`, `rgb()` и `rgba()`; остальное
 * (color-mix, имена) -- null: тест не притворяется браузером. */
export function parseColor(value: string): RGBA | null {
  const text = value.trim();
  const hex = text.match(/^#([0-9a-f]{3,8})$/i);
  if (hex) {
    const h = hex[1];
    if (h.length === 3 || h.length === 4) {
      return {
        r: channel(h[0]),
        g: channel(h[1]),
        b: channel(h[2]),
        a: h.length === 4 ? channel(h[3]) / 255 : 1,
      };
    }
    if (h.length === 6 || h.length === 8) {
      return {
        r: channel(h.slice(0, 2)),
        g: channel(h.slice(2, 4)),
        b: channel(h.slice(4, 6)),
        a: h.length === 8 ? channel(h.slice(6, 8)) / 255 : 1,
      };
    }
    return null;
  }
  const fn = text.match(/^rgba?\(\s*([^)]+)\)$/i);
  if (fn) {
    const parts = fn[1].split(/[\s,/]+/).filter(Boolean).map(Number);
    if (parts.length < 3 || parts.some((n) => !Number.isFinite(n))) return null;
    const [r, g, b, a = 1] = parts;
    return { r, g, b, a };
  }
  return null;
}

/** Полупрозрачный цвет поверх непрозрачного фона. */
export function composite(fg: RGBA, bg: RGBA): RGBA {
  const mix = (f: number, b: number) => Math.round(f * fg.a + b * (1 - fg.a));
  return { r: mix(fg.r, bg.r), g: mix(fg.g, bg.g), b: mix(fg.b, bg.b), a: 1 };
}

const linear = (c: number) => {
  const s = c / 255;
  return s <= 0.03928 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
};

/** Относительная яркость WCAG 2.x. */
export function luminance(color: RGBA): number {
  return 0.2126 * linear(color.r) + 0.7152 * linear(color.g) + 0.0722 * linear(color.b);
}

/** Контраст WCAG: текст с альфой сначала ложится на фон. */
export function contrastRatio(fg: RGBA, bg: RGBA): number {
  const text = fg.a < 1 ? composite(fg, bg) : fg;
  const l1 = luminance(text);
  const l2 = luminance(bg);
  const [light, dark] = l1 >= l2 ? [l1, l2] : [l2, l1];
  return (light + 0.05) / (dark + 0.05);
}

/** Контраст пары токенов темы; фон с альфой ложится на страницу
 * (`--bg-secondary`). Нераспознанный цвет -- отказ с именем токена. */
export function tokenContrast(tokens: TokenMap, fg: string, bg: string): number {
  const text = parseColor(resolveToken(tokens, fg));
  const surface = parseColor(resolveToken(tokens, bg));
  const page = parseColor(resolveToken(tokens, '--bg-secondary'));
  if (!text) throw new Error(`${fg} is not a plain color: ${resolveToken(tokens, fg)}`);
  if (!surface) throw new Error(`${bg} is not a plain color: ${resolveToken(tokens, bg)}`);
  const solid = surface.a < 1 && page ? composite(surface, page) : surface;
  return contrastRatio(text, solid);
}
