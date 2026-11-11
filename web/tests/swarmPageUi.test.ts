// FEATURE: The swarm page is the primary panel object (#215, epic #204).
// SPEC: The rendered page is titled by the swarm; known names replace IDs;
// the full swarm ID is a separate copy element; infrastructure and
// execution are separate; stopped and waiting read differently; the
// selected node shows its instruction, outcome and actor; /swarms/:id
// opens exactly the addressed swarm; the swarm list leads to the page.
// The goal of the current pass stands under the states as text, line breaks
// kept; the step history splits into passes under their goals (#242).
import { afterAll, describe, expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { act, createElement, type ReactNode } from 'react';
import { createRoot } from 'react-dom/client';
import { renderToStaticMarkup } from 'react-dom/server';
import { createInstance } from 'i18next';
import { I18nextProvider } from 'react-i18next';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import type { AccessSwarm } from '@/services/brood/access';
import type { NodeStatus, PipelineRun, RunGraph, RunNode } from '@/services/brood/pipelines';
import { SwarmView } from '@/features/swarms/SwarmPage';
import { installTestDOM } from './helpers/dom';
import en from '@/i18n/locales/en.json';

// DOM-глобалы на время файла: RunGraph разбирает серверный SVG.
const restoreDOM = installTestDOM();
afterAll(restoreDOM);

const i18n = createInstance();
await i18n.init({ lng: 'en', resources: { en: { translation: en } } });

const source = (path: string) => readFileSync(new URL(`../src/${path}`, import.meta.url), 'utf8');

const swarm: AccessSwarm = {
  id: 'swarm-7f3a9c2e5b1d4e6f',
  project_id: 'project-4b8c1d2e',
  owner_id: 'user-9a8b7c6d',
  owner_username: 'alice',
  role: 'owner',
  name: 'release-crew',
  atespace_name: 'brood-7f3a9c2e',
  state: 'active',
  created_at: '2026-11-01T09:00:00Z',
  updated_at: '2026-11-02T09:00:00Z',
};

const svg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 120 80">
<g class="node node-actor state-done" data-node-id="architecture"><rect x="0" y="0" width="50" height="20"/><text x="25" y="14">architecture</text></g>
<g class="node node-human state-active" data-node-id="decision"><rect x="0" y="40" width="50" height="20"/><text x="25" y="54">decision</text></g>
</svg>`;

const graph: RunGraph = {
  svg,
  nodes: [
    { id: 'architecture', kind: 'actor', prompt: 'Propose the architecture' },
    { id: 'decision', kind: 'human', question: 'Approve the proposal?' },
  ],
};

const run = (over: Partial<PipelineRun> = {}): PipelineRun => ({
  id: swarm.id,
  name: 'swarm-architecture',
  status: 'waiting_human',
  swarmId: swarm.id,
  swarmName: swarm.name,
  projectId: swarm.project_id,
  projectName: 'brood',
  owner: 'alice',
  startedAt: null,
  currentNode: 'decision',
  nodes: [
    {
      id: 'architecture',
      kind: 'actor',
      label: 'architecture',
      status: 'done',
      outcome: 'success',
      actorName: 'architect',
      sequence: 1,
    },
    { id: 'decision', kind: 'human', label: 'decision', status: 'active', sequence: 2 },
  ],
  ...over,
});

const retry = async () => {};
type ViewProps = Parameters<typeof SwarmView>[0];
const props = (over: Partial<ViewProps> = {}): ViewProps => ({
  swarm,
  projectName: 'brood',
  run: run(),
  runLoading: false,
  runError: '',
  retryRun: retry,
  graph,
  graphLoading: false,
  graphError: '',
  retryGraph: retry,
  selected: 'decision',
  onSelect: () => {},
  ...over,
});

const wrap = (node: ReactNode) =>
  createElement(I18nextProvider, { i18n }, createElement(MemoryRouter, null, node));
const wrapAt = (entry: string, node: ReactNode) =>
  createElement(
    I18nextProvider,
    { i18n },
    createElement(MemoryRouter, { initialEntries: [entry] }, node)
  );
const render = (over: Partial<ViewProps> = {}) =>
  renderToStaticMarkup(wrap(createElement(SwarmView, props(over))));

/** Текст разметки без тегов и атрибутов: то, что видит человек. */
const visibleText = (html: string) => html.replace(/<[^>]*>/g, ' ');

describe('identity', () => {
  test('the page is titled by the swarm, never by the graph definition', () => {
    const html = render();
    const title = html.match(/<h1[^>]*>(.*?)<\/h1>/)?.[1];
    expect(title).toBe('release-crew');
    expect(title).not.toContain('swarm-architecture');
  });

  test('known names of the project, owner and actor are shown instead of IDs', () => {
    const html = render({ selected: 'architecture' });
    const text = visibleText(html);
    expect(text).toContain('brood');
    expect(text).toContain('alice');
    expect(text).toContain('architect');
    expect(text).not.toContain(swarm.project_id);
    expect(text).not.toContain(swarm.owner_id);
    // The project is a link to its page, named, with the ID only in the address.
    expect(html).toContain(`href="/projects/${swarm.project_id}"`);
  });

  test('the full swarm ID is a separate copy element, not the title', () => {
    const html = render();
    const text = visibleText(html);
    expect(text.split(swarm.id).length - 1).toBe(1);
    expect(html).toMatch(new RegExp(`<code[^>]*>${swarm.id}</code>`));
    expect(html).toContain(`aria-label="${en.swarm_page.copy_id}"`);
  });
});

/** Блок «что сейчас» (#253): от его атрибута до инбокса или графа. */
const now = (html: string) => {
  const start = html.indexOf('data-swarm-now');
  if (start < 0) throw new Error('no "what now" block');
  const ends = [html.indexOf('data-inbox', start), html.indexOf(`>${en.swarm_page.graph}<`, start)]
    .filter((index) => index > start);
  return html.slice(start, ends.length > 0 ? Math.min(...ends) : undefined);
};
/** Строка-мета под заголовком: сведения о рое. */
const meta = (html: string) => {
  const start = html.indexOf('data-swarm-meta');
  if (start < 0) throw new Error('no meta row');
  return html.slice(start, html.indexOf('data-swarm-now', start));
};

describe('state', () => {
  // #253: infrastructure and execution are no longer two cards: the
  // infrastructure is a badge in the meta row, the execution is the first line.
  test('the infrastructure badge sits in the meta row; the execution opens the page', () => {
    const html = render();
    expect(meta(html)).toContain(en.access.states.active);
    expect(meta(html)).not.toContain(en.swarm_page.execution_states.waiting);
    expect(now(html)).toContain(en.swarm_page.execution_states.waiting);
    expect(now(html)).not.toContain(en.access.states.active);
    expect(html).not.toContain(`>${en.swarm_page.pass_goal.label}<`);
  });

  test('a swarm whose infrastructure is not active says so under the meta row', () => {
    const active = render();
    expect(active).not.toContain('data-infrastructure-alert');
    const provisioning = render({ swarm: { ...swarm, state: 'provisioning' } });
    const alert = provisioning.slice(provisioning.indexOf('data-infrastructure-alert'));
    expect(alert.slice(0, alert.indexOf('</p>'))).toContain(en.access.states.provisioning);
  });

  test('a stopped swarm and a swarm waiting for an answer read differently', () => {
    const waiting = now(render());
    const stopped = now(render({ run: run({ status: 'stopped' }) }));
    expect(waiting).toContain(en.swarm_page.execution_hints.waiting);
    expect(stopped).toContain(en.swarm_page.execution_states.stopped);
    expect(stopped).toContain(en.swarm_page.execution_hints.stopped);
    expect(stopped).not.toContain(en.swarm_page.execution_states.waiting);
    // Тон значка различается тоже: executionTone в swarmPage.test.ts.
  });

  test('a swarm without a graph says so instead of drawing an empty graph', () => {
    const html = render({ run: null, graph: null, selected: undefined });
    expect(now(html)).toContain(en.swarm_page.execution_hints.no_graph);
    expect(html).not.toContain('role="group"');
    expect(html).not.toContain(en.swarm_page.steps);
  });
});

describe('first screen (#253)', () => {
  const inbox = createElement('section', { 'data-inbox': 'yes' });
  const stuck = run({
    status: 'running',
    currentNode: 'architecture',
    nodes: [
      {
        id: 'architecture',
        kind: 'actor',
        label: 'architecture',
        status: 'unconfirmed',
        actorName: 'architect',
        detail: 'workspace attestation outcome is unknown; workspace operation op-7 stays reserved',
        sequence: 1,
      },
    ],
  });

  test('the page opens with the execution line, then the open question, then the graph', () => {
    const html = render({ requests: inbox });
    const line = html.indexOf('data-swarm-now');
    const question = html.indexOf('data-inbox');
    const graph = html.indexOf(`>${en.swarm_page.graph}<`);
    expect(line).toBeGreaterThan(html.indexOf('</header>'));
    expect(question).toBeGreaterThan(line);
    expect(graph).toBeGreaterThan(question);
    // The goal of the pass belongs to the first screen, under the line.
    const withGoal = render({ requests: inbox, run: run({ goal: 'Fix issue #12' }) });
    expect(withGoal.indexOf('data-pass-goal')).toBeGreaterThan(withGoal.indexOf('data-swarm-now'));
    expect(withGoal.indexOf('data-pass-goal')).toBeLessThan(withGoal.indexOf('data-inbox'));
  });

  test('one line names the execution, the node it stands on and the state of that step', () => {
    const line = now(render({ run: stuck, selected: 'architecture' }));
    const text = visibleText(line).replace(/\s+/g, ' ');
    expect(text).toContain(en.swarm_page.execution_states.running);
    expect(line).toMatch(/<button[^>]*>architecture<\/button>/);
    expect(text).toContain(en.swarm_page.node_states.unconfirmed);
    // Nothing is said twice: the waiting execution does not repeat the step word.
    const waiting = visibleText(now(render()));
    expect(waiting.split(en.swarm_page.execution_states.waiting).length - 1).toBe(1);
    expect(now(render())).toMatch(/<button[^>]*>decision<\/button>/);
  });

  test('the advice for a stuck step stands in the first block, once, with the action beside it', () => {
    const action = createElement('button', { 'data-action': 'yes' }, 'act');
    const html = render({ run: stuck, selected: 'architecture', actions: action });
    const first = now(html);
    expect(first).toContain('role="note"');
    expect(first).toContain('bro dispatch architect --request-id');
    expect(html.split('role="note"').length - 1).toBe(1);
    expect(first).toContain('data-action="yes"');
    // The hint of a running graph would contradict the stuck step: the advice replaces it.
    expect(visibleText(first)).not.toContain(en.swarm_page.execution_hints.running);
    expect(html.slice(html.indexOf(`>${en.swarm_page.node}<`))).not.toContain('role="note"');
    // What to do comes first; how an administrator releases the workspace is
    // folded under its own summary, the command inside it.
    const release = first.slice(first.indexOf('<details'));
    expect(first.indexOf(en.swarm_page.step_guidance.unconfirmed)).toBeLessThan(
      first.indexOf('<details')
    );
    expect(release.slice(0, release.indexOf('>'))).not.toContain(' open');
    expect(release.slice(0, release.indexOf('</summary>'))).toContain(
      en.swarm_page.step_guidance.release_title
    );
    expect(release).toContain('bro dispatch architect --request-id');
  });

  test('the facts are one meta row under the title; the ID keeps its copy button', () => {
    const row = meta(render());
    const text = visibleText(row).replace(/\s+/g, ' ');
    expect(text).toContain('brood');
    expect(text).toContain('alice');
    expect(text).toContain(en.access.swarm_roles.owner);
    expect(text).toContain(swarm.atespace_name);
    expect(row).toContain(`aria-label="${en.swarm_page.copy_id}"`);
    expect(row).toMatch(new RegExp(`<code[^>]*>${swarm.id}</code>`));
    expect(row).not.toContain('<dl');
  });

  test('the graph and the node panel share one row above 1024 px; the panel sticks', () => {
    const page = source('features/swarms/SwarmPage.tsx');
    expect(page).toContain('className={local.work}');
    const styles = source('features/swarms/SwarmPage.module.scss');
    const work = styles.slice(styles.indexOf('.work {'));
    expect(work.slice(0, work.indexOf('}'))).toContain('grid-template-columns');
    expect(work.slice(0, work.indexOf('}'))).toContain('minmax(');
    expect(styles).toMatch(/\.workAside[^}]*position: sticky/);
    // The graph section comes first in the row; the node panel is its aside.
    // The roles stand after the row, before the history: not on the first screen.
    const html = render({ roles: createElement('section', { 'data-roles': 'yes' }) });
    expect(html.indexOf(`>${en.swarm_page.graph}<`)).toBeLessThan(
      html.indexOf(`>${en.swarm_page.node}<`)
    );
    expect(html.indexOf(`>${en.swarm_page.node}<`)).toBeLessThan(html.indexOf('data-roles'));
    expect(html.indexOf('data-roles')).toBeLessThan(html.indexOf(`>${en.swarm_page.steps}<`));
    // A swarm without a graph still shows its roles, under the inbox.
    expect(
      render({
        run: null,
        graph: null,
        selected: undefined,
        roles: createElement('section', { 'data-roles': 'yes' }),
      })
    ).toContain('data-roles');
  });

  test('the goal of the pass is one or two lines that unfold; the whole text is there', () => {
    const goal = 'Line one of the goal\nLine two\nLine three\nLine four';
    const html = render({ run: run({ goal }) });
    const card = html.slice(html.indexOf('data-pass-goal'));
    expect(card).toContain('data-collapsed');
    expect(card).toContain('Line four');
    expect(card).toContain(`aria-expanded="false"`);
  });

  test('a step in the history shows how long it took; the moments stay in its title', () => {
    const timed = run({
      nodes: [
        {
          id: 'architecture',
          kind: 'actor',
          label: 'architecture',
          status: 'done',
          sequence: 1,
          startedAt: '2026-11-03T09:00:00Z',
          finishedAt: '2026-11-03T09:10:00Z',
        },
        {
          id: 'decision',
          kind: 'human',
          label: 'decision',
          status: 'active',
          sequence: 2,
          startedAt: '2026-11-03T09:10:00Z',
        },
      ],
    });
    const html = render({ run: timed, selected: undefined });
    const history = html.slice(html.indexOf(`>${en.swarm_page.steps}<`));
    expect(visibleText(history)).toContain(
      en.swarm_page.duration.min.replace('{{count}}', '10')
    );
    expect(history).toMatch(new RegExp(`title="${en.swarm_page.started} [^"]+${en.swarm_page.finished} [^"]+"`));
    expect(history).not.toContain(`>${en.swarm_page.finished}<`);
    // A step still running has no duration yet: its start is named instead.
    expect(visibleText(history)).toContain(en.swarm_page.started);
  });

  test('the node panel says how many times a node was entered only when it was more than once', () => {
    const twice = run({
      nodes: [
        { id: 'architecture', kind: 'actor', label: 'architecture', status: 'done', sequence: 1 },
        { id: 'architecture', kind: 'actor', label: 'architecture', status: 'done', sequence: 2 },
      ],
    });
    const once = render({ selected: 'architecture' });
    expect(visibleText(once)).not.toContain(en.swarm_page.entered.replace('{{count}}', '1'));
    expect(visibleText(render({ run: twice, selected: 'architecture' }))).toContain(
      en.swarm_page.entered.replace('{{count}}', '2')
    );
  });
});

describe('selected node', () => {
  test('the graph marks the selected node and the details follow it', () => {
    const html = render({ selected: 'architecture' });
    expect(html).toMatch(
      /data-node-id="architecture"[^>]*aria-pressed="true"|aria-pressed="true"[^>]*data-node-id="architecture"/
    );
    expect(html).toContain('Propose the architecture');
    expect(html).not.toContain('Approve the proposal?');
  });

  // #259: the card of the request at its node carries no form; the one form is in the inbox.
  test('a human node shows its question; the request card appears only on that node', () => {
    const gate = createElement('article', { 'data-gate': 'yes' });
    expect(render({ selected: 'decision', gateNode: 'decision', gate })).toContain('data-gate');
    expect(render({ selected: 'decision', gateNode: 'decision', gate })).toContain(
      'Approve the proposal?'
    );
    expect(render({ selected: 'architecture', gateNode: 'decision', gate })).not.toContain(
      'data-gate'
    );
  });

  test('step moments appear only when the server recorded them', () => {
    expect(render()).not.toContain(`>${en.swarm_page.started}<`);
    const timed = run({
      nodes: [
        {
          id: 'architecture',
          kind: 'actor',
          label: 'architecture',
          status: 'done',
          sequence: 1,
          startedAt: '2026-11-03T09:00:00Z',
          finishedAt: '2026-11-03T09:10:00Z',
        },
      ],
    });
    expect(render({ run: timed, selected: 'architecture' })).toContain(
      `>${en.swarm_page.started}<`
    );
  });

  // BUG: An unconfirmed actor visit had no visible uncertainty in the timeline.
  // HYPOTHESIS: The timeline displayed outcomes but not the unconfirmed state.
  // SOLUTION: Show the localized unknown-outcome state, in words.
  // RESULT: FIXED in the run page; the step history keeps it after #217.
  // STATUS: FIXED
  test('an unconfirmed step says its outcome is unknown, not that it failed', () => {
    const unconfirmed = run({
      nodes: [
        {
          id: 'architecture',
          kind: 'actor',
          label: 'architecture',
          status: 'unconfirmed',
          sequence: 1,
        },
      ],
    });
    const text = visibleText(render({ run: unconfirmed, selected: undefined }));
    expect(text).toContain(en.swarm_page.node_states.unconfirmed);
    expect(text).not.toContain(en.swarm_page.node_states.failed);
  });
});

describe('actor step states (#212, #238)', () => {
  const sha = '0123456789abcdef0123456789abcdef01234567';
  const actorStep = (over: Partial<RunNode> = {}): RunNode => ({
    id: 'architecture',
    kind: 'actor',
    label: 'architecture',
    status: 'refused',
    actorName: 'architect',
    sequence: 1,
    ...over,
  });
  const stuck = (over: Partial<RunNode> = {}, nodes: RunNode[] = []) =>
    run({ status: 'running', currentNode: 'architecture', nodes: [...nodes, actorStep(over)] });
  const count = (text: string, part: string) => text.split(part).length - 1;
  /** Видимый текст с раскрытыми сущностями: апостроф перевода — не &#x27;. */
  const shown = (html: string) =>
    visibleText(html)
      .replace(/&#x27;/g, "'")
      .replace(/&quot;/g, '"')
      .replace(/&lt;/g, '<')
      .replace(/&gt;/g, '>')
      .replace(/&amp;/g, '&');
  /** Панель узла и история шагов: то, что ниже графа. */
  const work = (html: string) => shown(html.slice(html.indexOf(`>${en.swarm_page.node}<`)));
  const states = en.swarm_page.node_states as Record<string, string>;
  const guidance = en.swarm_page.step_guidance;

  test('every step state reads in words in the node panel and in the history', () => {
    for (const status of [
      'pending',
      'attesting',
      'delivering',
      'inflight',
      'active',
      'completed',
      'done',
      'refused',
      'unconfirmed',
      'stopped',
    ] as NodeStatus[]) {
      const text = work(render({ run: stuck({ status }), selected: 'architecture' }));
      expect({ status, word: states[status], shown: count(text, ` ${states[status]} `) }).toEqual({
        status,
        word: expect.any(String),
        shown: 2,
      });
    }
  });

  test('a refused step names its reason and says what to do once', () => {
    const detail = 'actor architect is not connected to the relay; no instruction was sent';
    const html = render({ run: stuck({ detail }), selected: 'architecture' });
    const text = shown(html);
    // The reason is on the node and on its step in the history.
    expect(count(text, detail)).toBe(2);
    expect(text).toContain(` ${en.swarm_page.reason} `);
    expect(count(text, guidance.refused)).toBe(1);
    expect(text).not.toContain(guidance.unconfirmed);
    expect(text).not.toContain('bro dispatch');
    expect(html).toContain('role="note"');
  });

  test('an unconfirmed step leaves the decision to the operator, with the lease release', () => {
    const detail =
      'workspace attestation outcome is unknown; workspace operation op-7 stays reserved until an operator releases it; no instruction was sent';
    const text = shown(
      render({ run: stuck({ status: 'unconfirmed', detail }), selected: 'architecture' })
    );
    // The operation ID is a monospace element; the detail itself is unchanged.
    expect(count(text.replace(/\s+/g, ' '), detail)).toBe(2);
    expect(count(text, guidance.unconfirmed)).toBe(1);
    expect(text).toContain(guidance.release);
    expect(text).toContain('bro dispatch architect --request-id');
    expect(text).toContain(`--action release --swarm ${swarm.id}`);
    expect(text).not.toContain(guidance.refused);
    expect(text).not.toContain(` ${states.failed} `);
  });

  test('a refusal an earlier pass already answered gives no instruction', () => {
    const detail = 'the step changed before workspace attestation; no instruction was sent';
    const later = run({
      status: 'running',
      currentNode: 'decision',
      nodes: [
        actorStep({ detail }),
        { id: 'decision', kind: 'human', label: 'decision', status: 'active', sequence: 2 },
      ],
    });
    const text = shown(render({ run: later, selected: 'architecture' }));
    expect(count(text, detail)).toBe(2);
    expect(text).not.toContain(guidance.refused);
  });

  test('the confirmed commit is short on screen, whole in its title and its copy button', () => {
    const html = render({
      run: stuck({ status: 'done', outcome: 'success', commit: sha }),
      selected: 'architecture',
    });
    const text = visibleText(html);
    expect(count(text, sha.slice(0, 8))).toBe(2);
    expect(text).not.toContain(sha);
    expect(count(html, `title="${sha}"`)).toBe(2);
    expect(count(html, `aria-label="${en.swarm_page.copy_commit}"`)).toBe(2);
    expect(text).toContain(en.swarm_page.commit);
  });

  test('the step report is text with its line breaks, never markup', () => {
    const report = 'Done: <b>branch</b> feat/x\nMR !12 & docs';
    const html = render({
      run: stuck({ status: 'done', outcome: 'success', report }),
      selected: 'architecture',
    });
    expect(html).not.toContain('<b>branch</b>');
    expect(html).toContain('Done: &lt;b&gt;branch&lt;/b&gt; feat/x\nMR !12 &amp; docs');
    // Full in the node panel, folded in the history.
    expect(count(html, '&lt;b&gt;branch')).toBe(2);
    expect(html).toContain('<details');
    expect(html).toContain(`>${en.swarm_page.report}</summary>`);
  });

  test('a step without reason, commit or report shows none of them', () => {
    const text = shown(render({ run: stuck({ status: 'inflight' }), selected: 'architecture' }));
    expect(text).not.toContain(` ${en.swarm_page.reason} `);
    expect(text).not.toContain(` ${en.swarm_page.commit} `);
    expect(text).not.toContain(` ${en.swarm_page.report} `);
  });
});

describe('pass goal', () => {
  const goal = 'Fix issue #12\n\t- keep the API\n<b>as is</b>';
  const passes = run({
    goal: 'Fix issue #13',
    nodes: [
      {
        id: 'architecture',
        kind: 'actor',
        label: 'architecture',
        status: 'done',
        sequence: 1,
        pass: 1,
        goal,
      },
      { id: 'decision', kind: 'human', label: 'decision', status: 'done', sequence: 2, pass: 1 },
      {
        id: 'architecture',
        kind: 'actor',
        label: 'architecture',
        status: 'active',
        sequence: 3,
        pass: 3,
        goal: 'Fix issue #13',
      },
    ],
  });
  const goalCard = (html: string) => {
    const start = html.indexOf('data-pass-goal');
    return start < 0 ? '' : html.slice(start, html.indexOf('</section>', start));
  };

  test('the goal of the current pass is markdown without HTML, line breaks kept', () => {
    const html = render({ run: run({ goal, nodes: passes.nodes.slice(0, 2) }) });
    const card = goalCard(html);
    expect(card).toContain('data-goal-text=""');
    // Markdown (#252): the author's tag is text, the line breaks stay.
    expect(card).toContain('<p>Fix issue #12\n- keep the API\n&lt;b&gt;as is&lt;/b&gt;</p>');
    expect(card).not.toContain('<b>');
    // The pass is named by its first step.
    expect(visibleText(card)).toContain(
      en.swarm_page.pass_goal.pass.replace('{{number}}', '1').replace('{{step}}', '1')
    );
  });

  test('the card follows the current pass, not an earlier one', () => {
    const card = goalCard(render({ run: passes }));
    expect(card).toContain('Fix issue #13');
    expect(card).not.toContain('Fix issue #12');
    expect(visibleText(card)).toContain(
      en.swarm_page.pass_goal.pass.replace('{{number}}', '2').replace('{{step}}', '3')
    );
  });

  test('a swarm that has not started a pass has no goal card', () => {
    expect(goalCard(render())).toBe('');
    expect(goalCard(render({ run: null, graph: null, selected: undefined }))).toBe('');
  });

  test('the goal text keeps its line breaks on screen', () => {
    // Markdown (#252) keeps the breaks inside a paragraph: the rule lives there.
    const styles = source('components/ui/Markdown.module.scss');
    const rule = styles.slice(styles.indexOf('  p {'));
    expect(rule.slice(0, rule.indexOf('}'))).toContain('white-space: pre-wrap');
  });

  test('the step history splits into passes, each under its own goal', () => {
    const html = render({ run: passes, selected: undefined });
    const history = html.slice(html.indexOf(`>${en.swarm_page.steps}<`));
    const first = history.indexOf('data-pass="1"');
    const second = history.indexOf('data-pass="3"');
    expect(first).toBeGreaterThan(0);
    expect(second).toBeGreaterThan(first);
    const earlier = history.slice(first, second);
    const later = history.slice(second);
    expect(earlier).toContain('&lt;b&gt;as is&lt;/b&gt;');
    expect(earlier).toContain('#1<');
    expect(earlier).toContain('#2<');
    expect(earlier).not.toContain('#3<');
    expect(later).toContain('#3<');
    expect(visibleText(later)).toContain(
      en.swarm_page.pass_goal.pass.replace('{{number}}', '2').replace('{{step}}', '3')
    );
    // #253: the goal of the current pass stands once, in the first block;
    // its pass in the history does not repeat it.
    expect(goalCard(html)).toContain('Fix issue #13');
    expect(html.split('Fix issue #13').length - 1).toBe(1);
  });

  // #253: earlier passes are history of the history: folded into their
  // heading and step count, while the current pass stands open.
  test('earlier passes fold into their heading; the current pass stands open', () => {
    const html = render({ run: passes, selected: undefined });
    const history = html.slice(html.indexOf(`>${en.swarm_page.steps}<`));
    const earlier = history.slice(history.indexOf('data-pass="1"'), history.indexOf('data-pass="3"'));
    const later = history.slice(history.indexOf('data-pass="3"'));
    expect(earlier).toMatch(/<details(?![^>]*\sopen)[^>]*>/);
    expect(later).toMatch(/<details[^>]*\sopen/);
    // The summary names the pass and how many steps it took; the heading stays a heading.
    const summary = earlier.slice(0, earlier.indexOf('</summary>'));
    expect(summary).toMatch(
      new RegExp(
        `<h[2-4][^>]*>${en.swarm_page.pass_goal.pass.replace('{{number}}', '1').replace('{{step}}', '1')}</h[2-4]>`
      )
    );
    expect(visibleText(summary)).toContain(en.swarm_page.pass_steps.replace('{{count}}', '2'));
    expect(visibleText(later.slice(0, later.indexOf('</summary>')))).toContain(
      en.swarm_page.pass_steps.replace('{{count}}', '1')
    );
    // The goal and the steps of a folded pass are still there to unfold.
    expect(earlier).toContain('&lt;b&gt;as is&lt;/b&gt;');
    expect(earlier).toContain('#2<');
    // A single pass is the current one: open.
    const single = render({ run: run({ goal, nodes: passes.nodes.slice(0, 2) }), selected: undefined });
    expect(single.slice(single.indexOf('data-pass="1"'))).toMatch(/<details[^>]*\sopen/);
  });

  test('steps without passes keep the plain history', () => {
    const html = render({ selected: undefined });
    expect(html).not.toContain('data-pass=');
    expect(html).toContain('#1<');
    expect(html).toContain('#2<');
  });
});

describe('route and list', () => {
  // MainRoutes целиком в bun test не импортируется (страницы квот требуют
  // настоящих CSS-модулей), поэтому путь берётся из таблицы маршрутов, а
  // открывает его настоящий SwarmPage.
  test('a direct address opens exactly the addressed swarm', async () => {
    const routes = source('router/MainRoutes.tsx');
    const path = routes.match(/path: '([^']+)', element: <SwarmPage \/>/)?.[1];
    expect(path).toBe('/swarms/:id');
    const { SwarmPage } = await import('@/features/swarms/SwarmPage');
    const requested: string[] = [];
    const originalFetch = globalThis.fetch;
    globalThis.fetch = (async (url: string) => {
      requested.push(url);
      const body =
        url === `/api/swarms/${encodeURIComponent('s/2')}`
          ? { ...swarm, id: 's/2', name: 'second-crew' }
          : url === '/api/projects'
            ? [{ id: swarm.project_id, name: 'brood' }]
            : { detail: 'not found' };
      return new Response(JSON.stringify(body), { status: 'detail' in body ? 404 : 200 });
    }) as typeof fetch;
    (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    try {
      await act(async () =>
        root.render(
          wrapAt(
            `/swarms/${encodeURIComponent('s/2')}`,
            createElement(Routes, null, [
              createElement(Route, { key: 'list', path: '/swarms', element: 'list' }),
              createElement(Route, { key: 'swarm', path, element: createElement(SwarmPage) }),
            ])
          )
        )
      );
      await act(async () => new Promise((resolve) => setTimeout(resolve, 0)));
      expect(requested).toContain('/api/swarms/s%2F2');
      expect(requested).not.toContain('/api/swarms/s');
      expect(container.querySelector('h1')?.textContent).toBe('second-crew');
      // Рой без графа: исполнение «не начат», граф не рисуется.
      expect(container.textContent).toContain(en.swarm_page.execution_hints.no_graph);
    } finally {
      await act(async () => root.unmount());
      container.remove();
      globalThis.fetch = originalFetch;
    }
  });

  test('the route table names the swarm page and members may open it', async () => {
    const { visibleAccessPath } = await import('@/features/access/permissions');
    expect(source('router/MainRoutes.tsx')).toContain(
      "path: '/swarms/:id', element: <SwarmPage />"
    );
    expect(visibleAccessPath('/swarms/:id', false)).toBe(true);
  });

  test('a row of the swarm list opens the swarm page, not a side panel', () => {
    const list = source('features/access/SwarmsPage.tsx');
    expect(list).toContain('onRowClick={(swarm) => navigate(swarmPath(swarm.id))}');
    expect(list).not.toContain('<EntityPanel');
    // Access and deletion stay available on the page, after the work.
    const page = source('features/swarms/SwarmPage.tsx');
    const management = page.indexOf('{management}');
    expect(management).toBeGreaterThan(page.indexOf('<StepHistory'));
  });
});
