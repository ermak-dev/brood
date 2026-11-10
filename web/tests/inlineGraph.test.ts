// FEATURE: Inline interactive run graph (#214, epic #204).
// SPEC: The graph is a document node, not an image: the server SVG is
// parsed with DOMParser, filtered to an SVG allowlist, and rendered
// through React. Nodes are keyboard buttons with visible focus; state
// colors come from the module's shared tokens for both the graph and the
// timeline; the theme resolves --pip-* variables without media queries
// inside the SVG.
import { afterAll, describe, expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import * as sass from 'sass';
import { createElement, act, type ReactNode } from 'react';
import { createRoot } from 'react-dom/client';
import { renderToStaticMarkup } from 'react-dom/server';
import { createInstance } from 'i18next';
import { I18nextProvider } from 'react-i18next';
import {
  parseServerSVG,
  svgNodeID,
  svgNodeKind,
  svgNodeLabel,
  SVG_ELEMENT_ALLOWLIST,
  SVG_ATTRIBUTE_ALLOWLIST,
} from '@/features/pipelines/svgFilter';
import { RunGraph } from '@/features/pipelines/RunDetailPage';
import { installTestDOM } from './helpers/dom';
import en from '@/i18n/locales/en.json';

const i18n = createInstance();
await i18n.init({ lng: 'en', resources: { en: { translation: en } } });

// Глобалы DOM включены на время файла: и прямые вызовы парсера, и
// статический рендер компонента работают как в браузере.
const restoreDOM = installTestDOM();
afterAll(restoreDOM);

const parse = (source: string) => parseServerSVG(source);

/** Ищет узел по data-node-id в глубину дерева элементов. */
const nodeOf = (source: string, id: string): ReactNode => {
  const visit = (node: ReactNode): ReactNode => {
    if (!node || typeof node !== 'object' || !('props' in node)) return null;
    if (svgNodeID(node) === id) return node;
    const children = (node as { props?: { children?: unknown } }).props?.children;
    const list = Array.isArray(children) ? children : children ? [children] : [];
    for (const child of list) {
      const found = visit(child as ReactNode);
      if (found) return found;
    }
    return null;
  };
  for (const root of parse(source)) {
    const found = visit(root);
    if (found) return found;
  }
  return null;
};

const serverSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 200 120" role="img">
<style>.node rect{fill:var(--pip-node-fill,#fff)}</style>
<g class="node node-start state-done" data-node-id="start"><rect x="0" y="0" width="96" height="40" rx="20"/><text x="48" y="25" text-anchor="middle">start</text></g>
<g class="node node-human state-active" data-node-id="ask"><rect x="0" y="80" width="96" height="40"/><text x="48" y="105" text-anchor="middle">ask</text></g>
</svg>`;

describe('SVG parser filter', () => {
  test('passes through a valid server SVG intact', () => {
    const nodes = parse(serverSVG);
    expect(nodes.length).toBe(1);
    expect(nodes[0].type).toBe('svg');
    expect(nodes[0].props.viewBox).toBe('0 0 200 120');
    const start = nodeOf(serverSVG, 'start');
    expect(start?.type).toBe('g');
    expect(svgNodeLabel(start)).toBe('start');
    expect(svgNodeKind(start)).toBe('start');
    expect(svgNodeID(start)).toBe('start');
  });

  test('drops script, foreignObject, event handlers, href and unknown elements', () => {
    const hostile = `<svg xmlns="http://www.w3.org/2000/svg" onload="steal()">
<script>alert(1)</script>
<foreignObject><body xmlns="http://www.w3.org/1999/xhtml"><p>html</p></body></foreignObject>
<a xlink:href="javascript:steal()">x</a>
<image href="https://evil.example/x.png"/>
<animate attributeName="x" xlink:href="#node"/>
<g class="node" data-node-id="ask" onclick="steal()" filter="url(#f)">
<rect width="10" height="10"/><text>ask</text>
<set attributeName="fill" to="red"/></g>
<linearGradient id="g"><stop offset="0"/></linearGradient>
</svg>`;
    const nodes = parse(hostile);
    const html = renderToStaticMarkup(createElement('div', null, ...nodes));
    expect(html).not.toContain('<script');
    expect(html).not.toContain('foreignObject');
    expect(html).not.toContain(' onload=');
    expect(html).not.toContain(' onclick=');
    expect(html).not.toContain('javascript:');
    expect(html).not.toContain('evil.example');
    expect(html).not.toContain('<a ');
    expect(html).not.toContain('<image');
    expect(html).not.toContain('<animate');
    expect(html).not.toContain('<set');
    expect(html).not.toContain('linearGradient');
    expect(html).toContain('data-node-id="ask"');
    // Разрешённые элементы остаются: узел пережил фильтр.
    expect(html).toContain('<rect');
    expect(html).toContain('>ask<');
  });

  test('the allowlist covers exactly the piprender vocabulary', () => {
    expect([...SVG_ELEMENT_ALLOWLIST].sort()).toEqual(
      ['g', 'path', 'polygon', 'polyline', 'rect', 'svg', 'text'].sort()
    );
    expect(SVG_ATTRIBUTE_ALLOWLIST.has('href')).toBe(false);
    expect(SVG_ATTRIBUTE_ALLOWLIST.has('xlink:href')).toBe(false);
  });

  test('a non-SVG root yields no nodes', () => {
    expect(parse('<html><body>no</body></html>')).toEqual([]);
    expect(parse('<div></div>')).toEqual([]);
    expect(parse('not xml at all')).toEqual([]);
  });

  test('server <style> never reaches the document, whatever it says', () => {
    const hostile = `<svg xmlns="http://www.w3.org/2000/svg"><style>body{display:none}#root{display:none}.node rect{fill:url(https://evil.example/x.svg)}@import url(https://evil.example/x.css);</style><g class="node"/></svg>`;
    const html = renderToStaticMarkup(createElement('div', null, ...parse(hostile)));
    expect(html).not.toContain('<style');
    expect(html).not.toContain('body{');
    expect(html).not.toContain('#root');
    expect(html).not.toContain('evil.example');
    expect(html).not.toContain('@import');
  });

  test('paint attributes and url() values cannot load external resources', () => {
    const hostile = `<svg xmlns="http://www.w3.org/2000/svg" fill="url(https://evil.example/a)"><g class="node" data-node-id="n" fill="url(https://evil.example/b)" stroke="url(#x)" transform="url(https://evil.example/c)"><rect width="1" height="1" fill="url(https://evil.example/d)" style="fill:url(https://evil.example/e)"/><text x="0" y="0">n</text></g></svg>`;
    const html = renderToStaticMarkup(createElement('div', null, ...parse(hostile)));
    expect(html).not.toContain('url(');
    expect(html).not.toContain('evil.example');
    expect(html).not.toContain('fill=');
    expect(html).not.toContain('stroke=');
    expect(html).not.toContain('style=');
    expect(html).toContain('data-node-id="n"');
  });

  test('foreign classes on nodes are dropped, engine classes stay', () => {
    const svg = `<svg xmlns="http://www.w3.org/2000/svg"><g class="node node-actor state-done sr-only fixed" data-node-id="a"><rect width="1" height="1"/><text>a</text></g></svg>`;
    const g = nodeOf(svg, 'a');
    expect(String(g?.props.className)).toBe('node node-actor state-done');
  });

  test('the filter does not touch module classNames on nodes', () => {
    const g = nodeOf(serverSVG, 'ask');
    expect(String(g?.props.className)).toContain('node-human');
    expect(String(g?.props.className)).toContain('state-active');
  });
});

const wrap = (el: ReactNode) => createElement(I18nextProvider, { i18n }, el);

/** Монтирует в happy-dom и возвращает контейнер. */
async function mount(el: ReactNode) {
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
  const container = document.createElement('div');
  document.body.appendChild(container);
  const root = createRoot(container);
  await act(async () => root.render(wrap(el)));
  return {
    container,
    rerender: (next: ReactNode) => act(async () => root.render(wrap(next))),
    unmount: () => act(async () => root.unmount()),
  };
}

const key = (target: Element, k: string) => {
  const event = new KeyboardEvent('keydown', { key: k, bubbles: true, cancelable: true });
  act(() => {
    target.dispatchEvent(event);
  });
  return event;
};

const buttons = (c: Element) => Array.from(c.querySelectorAll('[role="button"]'));

describe('inline graph component', () => {
  test('without onSelect nodes are neither buttons nor tab stops', async () => {
    const { container, unmount } = await mount(
      createElement(RunGraph, { svg: serverSVG, activeNode: 'ask' })
    );
    expect(container.querySelector('svg')).not.toBeNull();
    expect(container.querySelector('[role="button"]')).toBeNull();
    expect(container.querySelector('[tabindex]')).toBeNull();
    expect(container.querySelectorAll('[data-node-id]').length).toBe(2);
    await unmount();
  });

  test('the svg is a group, so button children stay visible to screen readers', async () => {
    const { container, unmount } = await mount(
      createElement(RunGraph, { svg: serverSVG, onSelect: () => {} })
    );
    const svg = container.querySelector('svg');
    expect(svg?.getAttribute('role')).not.toBe('img');
    expect(svg?.getAttribute('aria-label')).toBe('Run graph');
    expect(buttons(container).map((b) => b.getAttribute('aria-label'))).toEqual([
      'start, start',
      'ask, human',
    ]);
    expect(buttons(container).every((b) => b.getAttribute('tabindex') === '0')).toBe(true);
    await unmount();
  });

  test('click, Enter and Space select the node; other keys do not', async () => {
    const selected: string[] = [];
    const { container, unmount } = await mount(
      createElement(RunGraph, { svg: serverSVG, onSelect: (id: string) => selected.push(id) })
    );
    const [start, ask] = buttons(container);
    act(() => {
      (ask as unknown as HTMLElement).dispatchEvent(new MouseEvent('click', { bubbles: true }));
    });
    expect(selected).toEqual(['ask']);
    const enter = key(start, 'Enter');
    expect(selected).toEqual(['ask', 'start']);
    expect(enter.defaultPrevented).toBe(true);
    const space = key(ask, ' ');
    expect(selected).toEqual(['ask', 'start', 'ask']);
    // Space не должен прокручивать страницу.
    expect(space.defaultPrevented).toBe(true);
    const tab = key(ask, 'Tab');
    key(ask, 'a');
    expect(selected.length).toBe(3);
    expect(tab.defaultPrevented).toBe(false);
    await unmount();
  });

  test('selected and active are distinct, both semantically and visually', async () => {
    const { container, unmount } = await mount(
      createElement(RunGraph, {
        svg: serverSVG,
        activeNode: 'ask',
        selectedNode: 'start',
        onSelect: () => {},
      })
    );
    const [start, ask] = buttons(container);
    expect(ask.getAttribute('aria-current')).toBe('true');
    expect(ask.getAttribute('aria-pressed')).toBe('false');
    expect(start.getAttribute('aria-pressed')).toBe('true');
    expect(start.getAttribute('aria-current')).toBeNull();
    // Активное — класс состояния движка; выбранное — отдельный признак.
    expect(ask.classList.contains('state-active')).toBe(true);
    expect(start.classList.contains('state-active')).toBe(false);
    await unmount();
  });

  test('selection of a node that is not in the graph is ignored after the graph changes', async () => {
    const other = serverSVG.replace(/data-node-id="start"/, 'data-node-id="begin"');
    const { container, rerender, unmount } = await mount(
      createElement(RunGraph, { svg: serverSVG, selectedNode: 'start', onSelect: () => {} })
    );
    expect(buttons(container)[0].getAttribute('aria-pressed')).toBe('true');
    await rerender(
      createElement(RunGraph, { svg: other, selectedNode: 'start', onSelect: () => {} })
    );
    expect(buttons(container).some((b) => b.getAttribute('aria-pressed') === 'true')).toBe(false);
    await unmount();
  });

  test('an unknown node kind falls back to its raw name, not a missing key', async () => {
    const svg = `<svg xmlns="http://www.w3.org/2000/svg"><g class="node node-quantum state-pending" data-node-id="q"><rect width="1" height="1"/><text>q</text></g></svg>`;
    const { container, unmount } = await mount(
      createElement(RunGraph, { svg, onSelect: () => {} })
    );
    const label = buttons(container)[0].getAttribute('aria-label');
    expect(label).toBe('q, quantum');
    expect(label).not.toContain('pipelines.kind');
    await unmount();
  });

  // BUG: .svg{width:100%} растягивал граф на ширину контейнера: на
  // десктопе узел вырастал в разы, на узком экране широкий граф сжимался,
  // и overflow-x контейнера не включался никогда (то же, что у <img>).
  // RESULT: в Chrome узел build шириной 96 в viewBox рисовался ~480px.
  test('the graph keeps the viewBox size, so a wide graph scrolls instead of scaling', async () => {
    const { container, unmount } = await mount(createElement(RunGraph, { svg: serverSVG }));
    const svg = container.querySelector('svg');
    expect(svg?.getAttribute('width')).toBe('200');
    expect(svg?.getAttribute('height')).toBe('120');
    await unmount();
  });

  test('a malformed viewBox gives no intrinsic size rather than NaN', async () => {
    const svg = serverSVG.replace('viewBox="0 0 200 120"', 'viewBox="0 0 wide -5"');
    const { container, unmount } = await mount(createElement(RunGraph, { svg }));
    const root = container.querySelector('svg');
    expect(root?.getAttribute('width')).toBeNull();
    expect(root?.getAttribute('height')).toBeNull();
    await unmount();
  });

  test('a hostile SVG mounts without script, style or handlers', async () => {
    const hostile =
      '<svg xmlns="http://www.w3.org/2000/svg" onload="steal()"><script>alert(1)</script><style>body{display:none}</style><g class="node" data-node-id="x" onclick="steal()" fill="url(https://evil.example/f)"><rect width="1" height="1"/><text>x</text></g></svg>';
    const { container, unmount } = await mount(createElement(RunGraph, { svg: hostile }));
    const html = container.innerHTML;
    expect(container.querySelector('script')).toBeNull();
    expect(container.querySelector('style')).toBeNull();
    expect(html).not.toContain('onload');
    expect(html).not.toContain('onclick');
    expect(html).not.toContain('evil.example');
    await unmount();
  });
});

describe('graph stylesheet', () => {
  test('the module paints engine classes and a selection rule distinct from state', () => {
    const css = sass.compile(
      fileURLToPath(new URL('../src/features/pipelines/pipelines.module.scss', import.meta.url))
    ).css;
    const rule = (needle: string) =>
      css.split('}').find((r) => r.includes(needle) && r.includes('{') && !r.includes(':hover')) ?? '';
    for (const state of ['active', 'done', 'failed', 'skipped']) {
      expect(css).toContain(`.state-${state}`);
    }
    // Выбранный узел: свой контур, не заливка состояния.
    const selected = rule('[aria-pressed=true]');
    expect(selected).toContain('stroke: var(--text-primary)');
    expect(selected).not.toContain('fill:');
    // Тёмная тема — через токены темы, не через медиа-запрос в графе.
    expect(css).not.toContain('prefers-color-scheme');
  });
});

describe('graph cascade', () => {
  const css = sass.compile(
    fileURLToPath(new URL('../src/features/pipelines/pipelines.module.scss', import.meta.url))
  ).css;
  const rules = css
    .split('}')
    .map((r) => r.split('{'))
    .filter((p) => p.length === 2)
    .map(([selector, body]) => ({ selector: selector.trim(), body }));

  // BUG: hover (специфичность 0,3,2) перебивал выбранный узел (0,2,2):
  // выбранный узел под курсором терял жирный контур.
  // RESULT: в Chrome stroke-width выбранного под hover был 2.5px вместо 3.5px.
  test('hover never applies to the selected node', () => {
    const hover = rules.filter((r) => r.selector.includes(':hover'));
    expect(hover.length).toBeGreaterThan(0);
    for (const rule of hover) {
      expect(rule.selector).toContain(":not([aria-pressed=true])");
    }
  });

  test('the svg is never stretched to the container width', () => {
    const svg = rules.find((r) => r.selector.endsWith('__svg') || /\.svg$/.test(r.selector));
    expect(svg).toBeDefined();
    expect(svg?.body).not.toContain('width: 100%');
    expect(svg?.body).toContain('max-width: none');
  });

  test('focus outline is drawn on the node rect, not removed with the group outline', () => {
    const focus = rules.find((r) => r.selector.includes(':focus-visible'));
    expect(focus?.selector).toContain('rect');
    expect(focus?.body).toContain('outline: 2px solid var(--primary-color)');
  });
});

describe('shared state tokens', () => {
  const module = readFileSync(
    new URL('../src/features/pipelines/pipelines.module.scss', import.meta.url),
    'utf8'
  );
  const themes = readFileSync(new URL('../src/styles/themes.scss', import.meta.url), 'utf8');

  test('timeline dots use the graph state variables', () => {
    expect(module).toContain('.timeline-done');
    expect(module).toContain('background: var(--pip-done-fill)');
    expect(module).toContain('.timeline-active');
    expect(module).toContain('background: var(--pip-active-fill)');
  });

  test('active and done no longer share one token', () => {
    expect(module).not.toMatch(/\.timeline-active[^}]*--viz-success/);
  });

  test('graph and timeline define one pip variable set without media queries', () => {
    // #215: подпись состояния узла на странице роя берёт тот же набор.
    expect(module).toContain('.graph,\n.timeline,\n.nodeState {');
    expect(module.match(/--pip-done-fill:/g)?.length).toBe(1);
    for (const token of [
      '--pip-node-fill',
      '--pip-active-fill',
      '--pip-done-fill',
      '--pip-failed-fill',
      '--pip-skipped-fill',
    ]) {
      expect(module).toContain(token);
    }
    // Тёмная тема не живёт внутри разметки: переменные ссылаются на
    // токены темы, которые переопределяет [data-theme='dark'].
    expect(module).not.toContain('prefers-color-scheme');
    expect(themes).toMatch(/\[data-theme='dark'\][^]*--viz-success/s);
    expect(themes).toMatch(/\[data-theme='dark'\][^]*--amber-color/s);
    expect(themes).toMatch(/\[data-theme='dark'\][^]*--success-badge-bg/s);
  });
});
