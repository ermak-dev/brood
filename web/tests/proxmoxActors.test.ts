// FEATURE: Управляемые акторы Proxmox в существующем потоке создания.
// SPEC: Явные phase/placement; stop/resume сохраняют workspace, reset уничтожает;
// native protojson и decimal int64 не меняются, просмотр не запускает актор.
// IMPLEMENTATION: Отдельные managed lifecycle и DTO, профиль в существующем редакторе.
// RESULT: IMPLEMENTED — явные действия, подтверждение reset и совместимость native проверены.
// STATUS: DONE
import { afterEach, beforeEach, expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createInstance } from 'i18next';
import { I18nextProvider } from 'react-i18next';
import {
  actorState,
  actorActions,
  actorCreateDraft,
  resetConfirmation,
  deleteConfirmation,
  canViewActorDetails,
} from '@/features/substrate/actorLifecycle';
import { ManagedProfile } from '@/features/substrate/components/ManagedProfile';
import {
  actorsApi,
  templatesApi,
  type Actor,
  type ActorTemplate,
} from '@/services/brood/substrate';
import en from '../src/i18n/locales/en.json';
import { ActorLifecycleActions } from '@/features/substrate/components/ActorLifecycleActions';

const capabilities = {
  workspace: true,
  terminal: true,
  controlledNetwork: true,
  docker: false,
  kind: false,
  memoryCheckpoint: false,
};
const template: ActorTemplate = {
  metadata: { name: 'claude-proxmox-limited', version: '9007199254740993' },
  provider: 'proxmox',
  profile: 'claude-proxmox',
  readOnly: true,
  capabilities,
  workerSelector: {
    matchLabels: { 'ate.dev/worker-pool': 'limited', 'brood.dev/provider': 'proxmox' },
  },
};
const actor: Actor = {
  metadata: { name: 'test', uid: 'managed', version: '9007199254740993' },
  provider: 'proxmox',
  profile: 'claude-proxmox',
  generation: '9007199254740995',
  phase: 'stopped',
  desired: 'stopped',
  placement: { node: 'pve-a', ctid: 105, pool: 'limited' },
  capabilities,
  status: { state: 'ACTOR_STATE_UNSPECIFIED' },
};
const originalFetch = globalThis.fetch;
const originalDocument = Object.getOwnPropertyDescriptor(globalThis, 'document');
beforeEach(() => {
  Object.defineProperty(globalThis, 'document', {
    configurable: true, value: { cookie: 'csrf=managed-actor-test' },
  });
});
afterEach(() => {
  globalThis.fetch = originalFetch;
  if (originalDocument) Object.defineProperty(globalThis, 'document', originalDocument);
  else Reflect.deleteProperty(globalThis, 'document');
});

test('managed phase wins over the native enum projection, including transitions and errors', () => {
  for (const phase of [
    'provisioning',
    'running',
    'stopping',
    'stopped',
    'deleting',
    'error',
  ] as const) {
    expect(actorState({ ...actor, phase, status: { state: 'ACTOR_STATE_SUSPENDED' } })).toBe(phase);
  }
  expect(
    actorState({ metadata: { name: 'native' }, status: { state: 'ACTOR_STATE_SUSPENDED' } })
  ).toBe('suspended');
  expect(actorState({ metadata: { name: 'empty' } })).toBe('');
  expect(actorState({ ...actor, phase: undefined })).toBe('');
});

test('only explicit appropriate managed actions are offered, never memory suspend', () => {
  expect(actorActions(actor)).toEqual(['resume', 'reset']);
  expect(actorActions({ ...actor, phase: 'running' })).toEqual(['stop', 'reset']);
  expect(actorActions({ ...actor, phase: 'error' })).toEqual(['resume', 'stop', 'reset']);
  for (const phase of ['provisioning', 'stopping', 'deleting'] as const) {
    expect(actorActions({ ...actor, phase })).toEqual([]);
  }
  expect(
    actorActions({ metadata: { name: 'native' }, status: { state: 'ACTOR_STATE_RUNNING' } })
  ).toEqual(['suspend']);
  expect(
    actorActions({ metadata: { name: 'native' }, status: { state: 'ACTOR_STATE_SUSPENDED' } })
  ).toEqual(['resume']);
});

test('virtual template create does not send native fields or a fake worker assignment', () => {
  expect(actorCreateDraft('swarm-id', 'test', template, {}, 'old-snapshot')).toEqual({
    metadata: { name: 'test' },
    actorTemplate: { name: template.metadata.name },
  });
  expect(actorCreateDraft('swarm-id', 'test', template, { team: 'a' }, '')).toEqual({
    metadata: { name: 'test' },
    actorTemplate: { name: template.metadata.name },
    workerSelector: { matchLabels: { team: 'a' } },
  });
  expect(
    actorCreateDraft(
      'swarm-id',
      'test',
      { metadata: { name: 'native' }, containers: [] },
      {},
      'tag'
    )
  ).toMatchObject({
    actorTemplate: { name: 'native', atespace: 'swarm-id' },
    sourceTag: { name: 'tag', atespace: 'swarm-id' },
  });
});

test('reset confirmation names destructive fresh CT and workspace loss, and waits for confirmation', async () => {
  const calls: string[] = [];
  const i18n = createInstance();
  await i18n.init({ lng: 'en', resources: { en: { translation: en } } });
  const confirmation = resetConfirmation(actor, i18n.t, async () => {
    calls.push('reset');
  });
  expect(confirmation.message).toContain('test');
  expect(confirmation.message).toContain('workspace');
  expect(confirmation.message).toContain('fresh container');
  expect(confirmation.message).toContain('permanently');
  expect(calls).toEqual([]);
  await confirmation.run();
  expect(calls).toEqual(['reset']);
});

test('viewers can inspect managed details without native edit or lifecycle permission', () => {
  expect(canViewActorDetails(false, actor)).toBe(true);
  expect(canViewActorDetails(false, { metadata: { name: 'native' } })).toBe(false);
  expect(canViewActorDetails(false, null)).toBe(false);
  expect(canViewActorDetails(true, null)).toBe(true);
  const source = readFileSync(new URL('../src/features/substrate/ActorsPage.tsx', import.meta.url), 'utf8');
  expect(source).toContain('canViewActorDetails(canManage, editor.actor)');
  expect(source).toContain('canViewActorDetails(canManage, a)');
});

test('managed deletion warns permanent workspace loss before submitting, native copy stays unchanged', async () => {
  const i18n = createInstance();
  await i18n.init({ lng: 'en', resources: { en: { translation: en } } });
  const calls: string[] = [];
  const confirmation = deleteConfirmation(actor, i18n.t, async () => { calls.push('delete'); });
  expect(confirmation.message).toContain('permanently');
  expect(confirmation.message).toContain('workspace');
  expect(confirmation.message).not.toContain('snapshots');
  expect(confirmation.success).toContain('requested');
  expect(calls).toEqual([]);
  await confirmation.run();
  expect(calls).toEqual(['delete']);
  expect(deleteConfirmation({ metadata: { name: 'native' } }, i18n.t, async () => {}).message).toContain('snapshots');
  for (const locale of ['en', 'ru', 'ko', 'vi', 'zh-CN', 'zh-TW']) {
    const messages = JSON.parse(readFileSync(new URL(`../src/i18n/locales/${locale}.json`, import.meta.url), 'utf8'));
    expect(messages.actors.managed_delete_message).toBeTruthy();
  }
});

test('managed details show real placement and limitations without service counters', async () => {
  const i18n = createInstance();
  await i18n.init({ lng: 'en', resources: { en: { translation: en } } });
  const html = renderToStaticMarkup(
    createElement(I18nextProvider, { i18n }, createElement(ManagedProfile, { entity: actor }))
  );
  for (const text of [
    'Proxmox',
    '105',
    'pve-a',
    'limited',
    'Docker',
    'kind',
    'memory checkpoint',
    'workspace',
  ]) {
    expect(html).toContain(text);
  }
  // The incarnation counter is an internal reconciliation detail, not user data.
  expect(html).not.toContain('9007199254740995');
  expect(html).not.toContain('Generation');
  expect(html).not.toContain('Worker');
});

test('rendering lifecycle controls never starts actors and gates terminal and unsupported actions', async () => {
  const i18n = createInstance();
  await i18n.init({ lng: 'en', resources: { en: { translation: en } } });
  const actions: string[] = [];
  const markup = (value: Actor, disabled = false) =>
    renderToStaticMarkup(
      createElement(
        I18nextProvider,
        { i18n },
        createElement(ActorLifecycleActions, {
          actor: value,
          swarm: 'swarm/a',
          disabled,
          onAction: (action: string) => {
            actions.push(action);
          },
        })
      )
    );
  const stopped = markup(actor);
  expect(stopped).toContain('Resume');
  expect(stopped).toContain('Reset workspace');
  expect(stopped).not.toContain('Suspend');
  expect(stopped).not.toContain('href=');
  const running = markup({ ...actor, phase: 'running' });
  expect(running).toContain('Stop');
  expect(running).not.toContain('Resume');
  expect(running).not.toContain('Suspend');
  expect(running).toContain('/api/swarms/swarm%2Fa/proxy/actors/test/');
  expect(markup({ ...actor, phase: 'stopping' })).not.toContain('<button');
  expect(markup({ ...actor, phase: 'deleting' })).not.toContain('href=');
  expect(markup(actor, true)).toContain('disabled');
  expect(
    markup({ metadata: { name: 'native' }, status: { state: 'ACTOR_STATE_RUNNING' } })
  ).toContain('Suspend');
  expect(actions).toEqual([]);
});

test('actions post only the exact managed intent and preserve server refusal reasons', async () => {
  const calls: Array<{ url: unknown; options: RequestInit }> = [];
  globalThis.fetch = (async (url, options) => {
    calls.push({ url, options: options! });
    return Response.json(actor);
  }) as typeof fetch;
  for (const action of ['stop', 'resume', 'reset'] as const) {
    expect(await actorsApi.act('swarm/a', 'actor/a', action)).toEqual(actor);
    expect(calls.at(-1)?.url).toBe('/api/swarms/swarm%2Fa/actors/actor%2Fa');
    expect(calls.at(-1)?.options.method).toBe('POST');
    expect(JSON.parse(calls.at(-1)!.options.body as string)).toEqual({ action });
  }
  globalThis.fetch = (async () =>
    Response.json({ detail: 'pool is draining' }, { status: 409 })) as typeof fetch;
  await expect(actorsApi.act('swarm', 'test', 'resume')).rejects.toMatchObject({
    status: 409,
    message: 'pool is draining',
  });
});

test('managed lifecycle and destructive confirmation are translated in all six locales', () => {
  const keys = [
    'provider',
    'profile',
    'pool',
    'details',
    'details_title',
    'meta_stopped',
    'stop',
    'stop_done',
    'stop_hint',
    'managed_limitations',
    'suspend_unsupported',
    'reset',
    'reset_title',
    'reset_message',
    'reset_done',
  ];
  for (const locale of ['en', 'ru', 'zh-CN', 'zh-TW', 'ko', 'vi']) {
    const messages = JSON.parse(
      readFileSync(new URL(`../src/i18n/locales/${locale}.json`, import.meta.url), 'utf8')
    ).actors;
    for (const key of keys) expect(messages[key]?.length).toBeGreaterThan(0);
    expect(messages.reset_message).toContain('{{name}}');
    for (const phase of ['provisioning', 'running', 'stopping', 'stopped', 'deleting', 'error']) {
      expect(messages.managed_state[phase]?.length).toBeGreaterThan(0);
    }
  }
});

test('list reads preserve decimal strings and do not perform implicit resume/reset', async () => {
  const calls: RequestInit[] = [];
  globalThis.fetch = (async (_url, options) => {
    calls.push(options!);
    return Response.json({ results: [actor] });
  }) as typeof fetch;
  expect((await actorsApi.list('swarm'))[0]).toEqual(actor);
  expect(calls.map((call) => call.method)).toEqual(['GET']);
  expect((await templatesApi.list('swarm'))[0].metadata.version).toBe('9007199254740993');
});
