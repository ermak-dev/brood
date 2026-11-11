// FEATURE: Heading hierarchy of substrate pages (#256, epic #251).
// SPEC: A page has one h1 (PageHeader); a Section inside the page is an h2
// by default, a Section nested in a Section is an h3, and headingLevel
// overrides both. A PageHeader without a meta line keeps the height of
// that line, so its actions never slide under the fixed header bar.
import { describe, expect, test } from 'bun:test';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { Section } from '@/features/substrate/components/form';
import { PageHeader } from '@/features/substrate/components/PageHeader';

const headings = (html: string) =>
  [...html.matchAll(/<(h[1-6])[^>]*>([^<]*)<\/\1>/g)].map((m) => [m[1], m[2]]);

describe('Section heading level', () => {
  test('a section inside a page is an h2 after the h1 of the header', () => {
    const html = renderToStaticMarkup(
      createElement(
        'div',
        null,
        createElement(PageHeader, { title: 'Swarm' }),
        createElement(Section, { title: 'Graph' }, 'body')
      )
    );
    expect(headings(html)).toEqual([
      ['h1', 'Swarm'],
      ['h2', 'Graph'],
    ]);
  });

  test('a section nested in a section is an h3, and its own children an h4', () => {
    const html = renderToStaticMarkup(
      createElement(
        Section,
        { title: 'Outer' },
        createElement(Section, { title: 'Inner' }, createElement(Section, { title: 'Deep' }, 'x'))
      )
    );
    expect(headings(html)).toEqual([
      ['h2', 'Outer'],
      ['h3', 'Inner'],
      ['h4', 'Deep'],
    ]);
  });

  test('headingLevel overrides the default and the nesting', () => {
    const html = renderToStaticMarkup(
      createElement(
        Section,
        { title: 'Outer', headingLevel: 3 },
        createElement(Section, { title: 'Inner', headingLevel: 3 }, 'x')
      )
    );
    expect(headings(html)).toEqual([
      ['h3', 'Outer'],
      ['h3', 'Inner'],
    ]);
  });
});

describe('PageHeader without meta', () => {
  test('keeps the meta line as a hidden placeholder, so actions stay below the fixed bar', () => {
    const html = renderToStaticMarkup(
      createElement(PageHeader, {
        title: 'Projects',
        actions: createElement('button', null, 'Create'),
      })
    );
    // Классы CSS-модулей в bun пустые: строка узнаётся по признакам.
    const placeholder = html.match(/<p[^>]*aria-hidden="true"[^>]*><\/p>/);
    expect(placeholder).not.toBeNull();
    expect(placeholder?.[0]).toContain('visibility:hidden');
  });

  test('with meta the line is visible and read', () => {
    const html = renderToStaticMarkup(
      createElement(PageHeader, { title: 'Swarm', meta: ['brood'] })
    );
    expect(html).not.toContain('aria-hidden');
    expect(html).not.toContain('visibility');
    expect(html).toContain('<p><span>brood</span></p>');
  });
});
