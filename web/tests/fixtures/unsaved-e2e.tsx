// Real-browser regression for #202: unsaved edits survive every history transition.
// Each page load runs one scenario (?run=<name>) with the Navigation API on or off (?nav=off).
// An outcome is either "prompted" (the editor stays, its URL and edit intact) or "restored"
// (the transition could not be cancelled, and reopening the editor offers the edit back).
// Browser Back/Forward and typing into the address bar come from tests/history.browser.ts
// over DevTools, so they are user-initiated as in a real browser.
import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { Outlet, RouterProvider, createHashRouter } from 'react-router-dom';
import i18n from '../../src/i18n';
import { AuthFilesOAuthModelAliasEditPage } from '../../src/pages/AuthFilesOAuthModelAliasEditPage';
import { identifyInitialHistoryEntry } from '../../src/router/initialHistoryEntry';
import { authFilesApi } from '../../src/services/api';
import { useAuthStore, useNotificationStore } from '../../src/stores';

const params = new URLSearchParams(location.search);
const scenarioName = params.get('run') ?? '';
const navigationApi = params.get('nav') !== 'off';
if (!navigationApi) {
  Object.defineProperty(window, 'navigation', { value: undefined, configurable: true });
}

// A reload discards the editor state; the scenario must notice it instead of timing out.
const loads = Number(sessionStorage.getItem('unsaved-e2e-loads') ?? 0) + 1;
sessionStorage.setItem('unsaved-e2e-loads', String(loads));

const EDITOR = '/auth-files/oauth-model-alias?provider=claude';
const BASELINE = 'baseline';
const EDIT = 'kept-alias';

Object.assign(authFilesApi, {
  list: async () => ({ files: [] }),
  getOauthExcludedModels: async () => ({}),
  getOauthModelAlias: async () => ({ claude: [{ name: 'claude-sonnet', alias: BASELINE }] }),
  getModelDefinitions: async () => [],
});
useAuthStore.setState({ connectionStatus: 'connected' });

identifyInitialHistoryEntry();
const Page = ({ name }: { name: string }) => <output>{name}</output>;
const router = createHashRouter([
  {
    element: <Outlet />,
    children: [
      { path: '/', element: <Page name="home" /> },
      { path: '/direct', element: <Page name="direct" /> },
      { path: '/other', element: <Page name="other" /> },
      { path: '/auth-files/oauth-model-alias', element: <AuthFilesOAuthModelAliasEditPage /> },
    ],
  },
]);
createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <RouterProvider router={router} />
  </StrictMode>
);

const routerPath = () => `${router.state.location.pathname}${router.state.location.search}`;
const addressPath = () => decodeURI(location.hash.slice(1)) || '/';
const aliasInput = () => document.querySelector<HTMLInputElement>('input[id$="-alias"]');
const confirmation = () => useNotificationStore.getState().confirmation;
const dialogTitle = () => (confirmation().isOpen ? confirmation().options?.title : undefined);
const frame = () => new Promise((resolve) => requestAnimationFrame(resolve));

async function settle() {
  await new Promise((resolve) => setTimeout(resolve, 150));
  await frame();
  await frame();
}

async function waitFor<T>(read: () => T | null | undefined | false, label: string): Promise<T> {
  const deadline = performance.now() + 4000;
  while (performance.now() < deadline) {
    const value = read();
    if (value) return value;
    await frame();
  }
  throw new Error(
    `${label} (router ${routerPath()}, address ${addressPath()}, ` +
      `editor ${aliasInput()?.value ?? 'closed'}, dialog ${dialogTitle() ?? 'none'})`
  );
}

// The router state changes before React commits; another page also has to replace the editor.
const waitForPath = (path: string, label: string) =>
  waitFor(
    () => routerPath() === path && addressPath() === path && (path === EDITOR || !aliasInput()),
    label
  );

// The blocker sees a render only after its effects run, so settle after the DOM is ready.
async function waitForBaseline(label: string) {
  const input = await waitFor(() => {
    const field = aliasInput();
    return field && !field.disabled && field.value === BASELINE ? field : null;
  }, label);
  await settle();
  return input;
}

async function editAfter(open: () => unknown) {
  await open();
  await waitForPath(EDITOR, 'editor did not open');
  const input = await waitForBaseline('editor did not open with its baseline');
  const setValue = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!;
  setValue.call(input, EDIT);
  input.dispatchEvent(new Event('input', { bubbles: true }));
  await waitFor(() => aliasInput()?.value === EDIT, 'edit was not applied');
  await settle();
}

let requestSeq = 0;
async function browser(action: { go: number } | { url: string }) {
  const seq = String(++requestSeq);
  document.body.dataset.request = JSON.stringify({ seq, ...action });
  await waitFor(() => document.body.dataset.done === seq, `browser ${JSON.stringify(action)}`);
}
const browserBack = () => browser({ go: -1 });
const browserForward = () => browser({ go: 1 });
const typeAddress = (path: string) => browser({ url: `${location.href.split('#')[0]}#${path}` });

async function answer(confirm: boolean) {
  const options = confirmation().options;
  if (!options) throw new Error('no dialog to answer');
  if (confirm) await options.onConfirm();
  else options.onCancel?.();
  useNotificationStore.getState().hideConfirmation();
  await settle();
}

type Scenario = {
  setup: () => Promise<void>;
  away: () => unknown;
  awayPath: string;
  back: () => unknown;
};

async function openFromDirectEntry() {
  location.hash = '#/direct';
  await waitForPath('/direct', 'direct hash entry did not render');
  await editAfter(() => router.navigate(EDITOR));
}

const scenarios: Record<string, Scenario> = {
  // The issue: an external hash entry, then a router entry with the editor, then Back.
  issue: {
    setup: openFromDirectEntry,
    away: browserBack,
    awayPath: '/direct',
    back: browserForward,
  },
  // The editor itself opened by a direct hash change, then Back.
  'direct-editor': {
    setup: () => editAfter(() => (location.hash = `#${EDITOR}`)),
    away: browserBack,
    awayPath: '/',
    back: browserForward,
  },
  // Router entries only: Back.
  managed: {
    setup: () => editAfter(() => router.navigate(EDITOR)),
    away: browserBack,
    awayPath: '/',
    back: browserForward,
  },
  // Router entries only: Forward into an existing entry.
  forward: {
    setup: async () => {
      await router.navigate(EDITOR);
      await waitForBaseline('editor did not open');
      await router.navigate('/other');
      await waitForPath('/other', 'other page did not render');
      await editAfter(browserBack);
    },
    away: browserForward,
    awayPath: '/other',
    back: browserBack,
  },
  // A router link away from the editor.
  push: {
    setup: () => editAfter(() => router.navigate(EDITOR)),
    away: () => router.navigate('/other'),
    awayPath: '/other',
    back: browserBack,
  },
  // The panel's own Back button: navigate(-1), a scripted traversal.
  'app-back': {
    setup: () => editAfter(() => router.navigate(EDITOR)),
    away: () => router.navigate(-1),
    awayPath: '/',
    back: browserForward,
  },
  // A scripted Back from an entry the router cannot index.
  'script-back': {
    setup: openFromDirectEntry,
    away: () => history.back(),
    awayPath: '/direct',
    back: () => history.forward(),
  },
  // Another address typed into the address bar.
  'typed-hash': {
    setup: () => editAfter(() => router.navigate(EDITOR)),
    away: () => typeAddress('/other'),
    awayPath: '/other',
    back: browserBack,
  },
  // history.state writes are denied, so the external entry stays unidentified and the
  // router guesses the next index; Back by two entries.
  'write-denied': {
    setup: async () => {
      history.replaceState = () => {
        throw new DOMException('History state writes are denied', 'SecurityError');
      };
      await openFromDirectEntry();
    },
    away: () => browser({ go: -2 }),
    awayPath: '/',
    back: () => browser({ go: 2 }),
  },
};

async function expectNoLoss(scenario: Scenario): Promise<'prompted' | 'restored'> {
  const unsavedTitle = i18n.t('common.unsaved_changes_title');
  await scenario.setup();
  await scenario.away();
  await settle();
  if (dialogTitle() === unsavedTitle) {
    await waitForPath(EDITOR, 'confirmation left the address away from the editor');
    if (aliasInput()?.value !== EDIT) throw new Error('confirmation lost the edit');
    await answer(false);
    await waitForPath(EDITOR, 'staying left the editor');
    if (aliasInput()?.value !== EDIT) throw new Error('staying lost the edit');
    await scenario.away();
    await waitFor(() => dialogTitle() === unsavedTitle, 'second transition was not confirmed');
    await answer(true);
    await waitForPath(scenario.awayPath, 'leaving did not reach the target');
    await scenario.back();
    await waitForBaseline('editor did not reopen after leaving');
    await settle();
    if (dialogTitle()) throw new Error(`discarded edit was offered again: ${dialogTitle()}`);
    return 'prompted';
  }
  await waitForPath(scenario.awayPath, 'transition did not reach the target');
  await scenario.back();
  await waitFor(() => aliasInput(), 'editor did not reopen');
  await waitFor(
    () => dialogTitle() === i18n.t('common.unsaved_draft_title'),
    'unsaved edit lost: no restore offer'
  );
  await answer(true);
  await waitFor(() => aliasInput()?.value === EDIT, 'restore did not bring the edit back');
  return 'restored';
}

async function run() {
  const scenario = scenarios[scenarioName];
  if (!scenario) throw new Error(`unknown scenario ${scenarioName}`);
  if (loads > 1) throw new Error('page reloaded');
  // Until the document is completely loaded a hash change replaces the entry instead of adding one.
  if (document.readyState !== 'complete') {
    await new Promise((resolve) => addEventListener('load', resolve, { once: true }));
  }
  await settle();
  await waitForPath('/', 'home did not render');
  return expectNoLoss(scenario);
}

const label = `${scenarioName} nav=${navigationApi ? 'on' : 'off'}`;
void run().then(
  (outcome) => {
    document.body.dataset.result = `PASS ${label}: ${outcome}`;
  },
  (error: Error) => {
    document.body.dataset.result = `FAIL ${label}: ${error.message}`;
  }
);
