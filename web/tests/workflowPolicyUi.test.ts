import { afterAll, expect, test } from 'bun:test';
import { act, createElement } from 'react';
import { createRoot } from 'react-dom/client';
import { createInstance } from 'i18next';
import { I18nextProvider } from 'react-i18next';
import { MemoryRouter } from 'react-router-dom';
import {
  ProjectWorkflowPolicy,
  WorkflowPolicyEditor,
} from '@/features/access/ProjectWorkflowPolicy';
import type { ProjectWorkflowPolicy as Policy } from '@/services/brood/workflowPolicy';
import en from '@/i18n/locales/en.json';
import { installTestDOM } from './helpers/dom';

const restore = installTestDOM();
afterAll(restore);
(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
const i18n = createInstance();
await i18n.init({ lng: 'en', resources: { en: { translation: en } } });
const policy: Policy = {
  projectId: 'p',
  revision: 7,
  rules: [{ id: 'storage', paths: ['go/**'], require: 'human' }],
  createdBy: 'admin',
  createdAt: '2026-11-16T08:00:00Z',
};
const wrap = (node: ReturnType<typeof createElement>) =>
  createElement(I18nextProvider, { i18n }, createElement(MemoryRouter, null, node));

async function enter(field: HTMLTextAreaElement, value: string) {
  await act(async () => {
    Object.getOwnPropertyDescriptor(Object.getPrototypeOf(field) as object, 'value')?.set?.call(
      field,
      value
    );
    const props = Object.entries(field).find(([key]) => key.startsWith('__reactProps$'))?.[1] as
      { onChange: (event: { target: HTMLTextAreaElement }) => void } | undefined;
    if (!props?.onChange) throw new Error('React text input handler missing');
    props.onChange({ target: field });
  });
}

test('policy editor sends opened revision once and preserves draft after conflict', async () => {
  const original = globalThis.fetch;
  const writes: Array<{ url: string; body: unknown }> = [];
  globalThis.fetch = (async (input, init) => {
    if (init?.method === 'PUT') {
      writes.push({ url: String(input), body: JSON.parse(String(init.body)) });
      return new Response(
        JSON.stringify({ detail: 'workflow policy revision changed; reload before editing' }),
        { status: 409 }
      );
    }
    return new Response(JSON.stringify(policy), { status: 200 });
  }) as typeof fetch;
  const container = document.createElement('div');
  document.body.append(container);
  const root = createRoot(container);
  let closed = 0,
    saved = 0;
  try {
    await act(async () =>
      root.render(
        wrap(
          createElement(WorkflowPolicyEditor, {
            policy,
            close: () => {
              closed++;
            },
            saved: async () => {
              saved++;
            },
          })
        )
      )
    );
    const field = document.querySelector('textarea')!;
    expect(field).not.toBeNull();
    const draft = '- id: deployment\n  actions: [deploy]\n  require: human';
    await enter(field, draft);
    const save = [...document.querySelectorAll('button')].find(
      (x) => x.textContent === en.substrate.save
    )!;
    expect(save.disabled).toBe(false);
    await act(async () => save.click());
    expect(writes).toEqual([
      {
        url: '/api/projects/p/workflow-policy',
        body: { revision: 7, rules: [{ id: 'deployment', actions: ['deploy'], require: 'human' }] },
      },
    ]);
    expect(field.value).toBe(draft);
    expect(document.body.textContent).toContain(
      'workflow policy revision changed; reload before editing'
    );
    expect(closed).toBe(0);
    expect(saved).toBe(0);
    await enter(field, '- id: broken\n  paths: [');
    expect(save.disabled).toBe(true);
    expect(writes).toHaveLength(1);
  } finally {
    await act(async () => root.unmount());
    container.remove();
    globalThis.fetch = original;
  }
});

test('project policy is readable without exposing editor controls to a member', async () => {
  const original = globalThis.fetch;
  const calls: string[] = [];
  globalThis.fetch = (async (input) => {
    calls.push(String(input));
    return new Response(JSON.stringify(policy), { status: 200 });
  }) as typeof fetch;
  const container = document.createElement('div');
  document.body.append(container);
  const root = createRoot(container);
  try {
    await act(async () =>
      root.render(wrap(createElement(ProjectWorkflowPolicy, { projectId: 'p', canEdit: false })))
    );
    expect(container.textContent).toContain('Workflow policy');
    expect(container.textContent).toContain('7');
    expect(container.textContent).toContain('storage');
    expect(container.textContent).toContain('New passes only');
    expect(container.querySelector('textarea')).toBeNull();
    expect(
      [...container.querySelectorAll('button')].some((x) => x.textContent === 'Edit policy')
    ).toBe(false);
    expect(calls).toEqual(['/api/projects/p/workflow-policy']);
  } finally {
    await act(async () => root.unmount());
    container.remove();
    globalThis.fetch = original;
  }
});
