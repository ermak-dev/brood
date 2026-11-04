// FEATURE: Две вкладки актора и терминал прямо в списке (#159).
// SPEC: Обзор содержит сведения, Действия — операции. Лишних вкладок и
// служебных пояснений нет. Терминал в строке соблюдает права и состояние.
import { expect, test } from 'bun:test';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createInstance } from 'i18next';
import { I18nextProvider } from 'react-i18next';
import { ActorDetailsSheet } from '@/features/substrate/ActorDetailsSheet';
import { ActorTable } from '@/features/substrate/ActorsPage';
import type { Actor } from '@/services/brood/substrate';
import en from '../src/i18n/locales/en.json';

const actor: Actor = {
  metadata: { name: 'coding', uid: 'actor-1' },
  provider: 'proxmox',
  phase: 'running',
  desired: 'running',
  role: 'worker',
  generation: '1',
  origin: 'https://git.example.com/repo',
  originMode: 'gateway',
  capabilities: {
    workspace: true,
    terminal: true,
    controlledNetwork: true,
    docker: false,
    kind: false,
    memoryCheckpoint: false,
  },
};
async function render(component: Parameters<typeof createElement>[0], props: object) {
  const i18n = createInstance();
  await i18n.init({ lng: 'en', resources: { en: { translation: en } } });
  return renderToStaticMarkup(
    createElement(I18nextProvider, { i18n }, createElement(component, props))
  );
}
const details = {
  open: true,
  onClose: () => {},
  actor,
  swarm: 'swarm',
  swarmName: 'team',
  canManage: true,
  administrator: false,
  failure: null,
  unknown: [],
  onAction: () => {},
  onClearFailure: () => {},
  onDismissUnknown: () => {},
};

test('only overview and actions tabs remain, with facts separate from operations', async () => {
  const html = await render(ActorDetailsSheet, details);
  const tabs = [...html.matchAll(/role="tab"[^>]*>([^<]+)</g)].map((m) => m[1]);
  expect(tabs).toEqual(['Overview', 'Actions']);
  const overview = html.slice(
    html.indexOf('role="tabpanel"'),
    html.indexOf('role="tabpanel"', html.indexOf('role="tabpanel"') + 1)
  );
  expect(overview).toContain(actor.origin!);
  expect(overview).not.toContain('>Restart Claude<');
  expect(overview).not.toContain('>Recreate<');
  expect(html).not.toContain('Open terminal');
  for (const text of [
    en.actors.managed_limitations,
    en.actors.stop_hint,
    en.actors.suspend_unsupported,
  ]) {
    expect(html).not.toContain(text);
  }
});

test('a running actor has a direct terminal link in the table, not an operation button', async () => {
  const html = await render(ActorTable, {
    rows: [actor],
    swarm: 'swarm',
    canManage: true,
    goldenOf: () => undefined,
    onOpen: () => {},
  });
  expect(html).toContain('href="/api/swarms/swarm/proxy/actors/coding/"');
  expect(html).toContain('target="_blank"');
  expect(html).toContain('Open terminal');
  expect(html).not.toContain('>Stop<');
  expect(html).not.toContain('>Delete<');
});

test('viewers and golden actors have no terminal link; stopped actors have no live link', async () => {
  for (const props of [
    { rows: [actor], canManage: false, goldenOf: () => undefined },
    { rows: [actor], canManage: true, goldenOf: () => 'snapshot' },
    {
      rows: [{ ...actor, phase: 'stopped', desired: 'stopped' }],
      canManage: true,
      goldenOf: () => undefined,
    },
  ]) {
    const html = await render(ActorTable, { ...props, swarm: 'swarm', onOpen: () => {} });
    expect(html).not.toContain('href=');
  }
});
