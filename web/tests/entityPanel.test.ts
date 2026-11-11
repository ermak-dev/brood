// FEATURE: Общая панель действий и строки без встроенных кнопок (#157).
// SPEC: Причина блокировки видна, pending не активируется, опасное после деталей;
// строки без разрешения не получают фокус и стрелку, вложенные действия сохранены.
import { expect, test } from 'bun:test';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createInstance } from 'i18next';
import { I18nextProvider } from 'react-i18next';
import { EntityPanel, PanelActions, type PanelActionGroup } from '../src/features/substrate/components/EntityPanel';
import { EntityTable } from '../src/features/substrate/components/EntityTable';
import en from '../src/i18n/locales/en.json';

async function render(element: ReturnType<typeof createElement>) {
  const i18n = createInstance();
  await i18n.init({ lng: 'en', resources: { en: { translation: en } } });
  return renderToStaticMarkup(createElement(I18nextProvider, { i18n }, element));
}

const groups: PanelActionGroup[] = [
  { id: 'danger', label: 'Irreversible', danger: true, actions: [{ id: 'delete', label: 'Delete', tone: 'danger' }] },
  { id: 'empty', label: 'Empty group', actions: [] },
  { id: 'main', label: 'Actions', actions: [
    { id: 'terminal', label: 'Terminal', href: '/terminal', description: 'New terminal' },
    { id: 'blocked', label: 'Unavailable', href: '/forbidden', blocked: 'No permission' },
    { id: 'pending', label: 'Starting', pending: true },
  ] },
];

test('panel puts irreversible actions after object details and omits empty groups', async () => {
  const html = await render(createElement(EntityPanel, {
    open: true, onClose: () => {}, title: 'Worker', actions: groups,
    children: createElement('p', null, 'Object details'),
  }));
  expect(html.indexOf('Terminal')).toBeLessThan(html.indexOf('Object details'));
  expect(html.indexOf('Object details')).toBeLessThan(html.indexOf('Irreversible'));
  expect(html).not.toContain('Empty group');
});

// #256: карточка-кнопка называлась всем своим текстом («Swarm access Shows
// who can…»): имя — заголовок, описание — через aria-describedby.
test('a card button is named by its label and described by its text', async () => {
  const html = await render(
    createElement(PanelActions, {
      groups: [
        {
          id: 'main',
          label: 'Actions',
          actions: [{ id: 'access', label: 'Swarm access', description: 'Shows who can use it.' }],
        },
      ],
    })
  );
  const button = html.match(/<button[^>]*>/)?.[0] ?? '';
  const labelledBy = button.match(/aria-labelledby="([^"]+)"/)?.[1];
  const describedBy = button.match(/aria-describedby="([^"]+)"/)?.[1];
  expect(labelledBy).toBeTruthy();
  expect(describedBy).toBeTruthy();
  expect(labelledBy).not.toBe(describedBy);
  expect(html).toMatch(new RegExp(`id="${labelledBy}"[^>]*>(<span[^>]*>[^<]*</span>)?Swarm access<`));
  expect(html).toMatch(new RegExp(`id="${describedBy}"[^>]*>Shows who can use it\\.<`));
});

test('blocked links become focusable unavailable buttons with visible reasons', async () => {
  const html = await render(createElement(PanelActions, { groups }));
  expect(html).toContain('href="/terminal"');
  expect(html).toContain('target="_blank"');
  expect(html).toContain('(opens in a new tab)');
  expect(html).not.toContain('href="/forbidden"');
  expect(html).toContain('No permission');
  expect(html.match(/aria-disabled="true"/g)).toHaveLength(2);
  expect(html).toContain('aria-busy="true"');
  expect(html).not.toMatch(/\sdisabled(?:[=>\s])/);
});

test('mixed rows only expose permitted panel entry points', () => {
  const html = renderToStaticMarkup(createElement(EntityTable<{ name: string; open: boolean }>, {
    columns: [{ key: 'name', label: 'Name', render: row => row.name }],
    rows: [{ name: 'viewable', open: true }, { name: 'restricted', open: false }],
    rowKey: row => row.name, onRowClick: () => {}, canOpen: row => row.open,
  }));
  const rows = html.slice(html.indexOf('<tbody')).split('<tr').slice(1);
  expect(rows[0]).toContain('tabindex="0"');
  expect(rows[0]).toContain('<svg');
  expect(rows[1]).not.toContain('tabindex');
  expect(rows[1]).not.toContain('<svg');
  expect(html).not.toMatch(/<(button|a)\b/);
});
