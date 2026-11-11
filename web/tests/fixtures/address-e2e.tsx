// Real-browser regression for #258: an address typed into the address bar opens its page.
// Each page load runs one scenario (?run=<name>) and starts at a swarm page, as a reload of
// that page does. The typed address comes from tests/history.browser.ts over DevTools, so the
// navigation is the browser's, as when a user types it. The audit typed it while the reloaded
// panel restored its session, and the restore then took the panel back to the address it had
// loaded with.
import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import {
  Outlet,
  RouterProvider,
  createHashRouter,
  useParams,
  useRoutes,
  type Location,
} from 'react-router-dom';
import '../../src/i18n';
import { PageTransition } from '../../src/components/common/PageTransition';
import { LoginPage } from '../../src/pages/LoginPage';
import { ProtectedRoute } from '../../src/router/ProtectedRoute';
import { identifyInitialHistoryEntry } from '../../src/router/initialHistoryEntry';
import { useAuthStore } from '../../src/stores';

const scenarioName = new URLSearchParams(location.search).get('run') ?? '';

// A reload discards the panel state; the scenario must notice it instead of passing.
const loads = Number(sessionStorage.getItem('address-e2e-loads') ?? 0) + 1;
sessionStorage.setItem('address-e2e-loads', String(loads));

const SWARM = '/swarms/s-docs';
const SWARMS = '/swarms';

// The session restore answers when the scenario lets it, like a server that takes its time.
let finishRestore = () => {};
const restore = new Promise<void>((resolve) => {
  finishRestore = resolve;
});
useAuthStore.setState({
  isAuthenticated: scenarioName === 'signed-in',
  managementKey: '',
  restoreSession: async () => {
    await restore;
    useAuthStore.setState({ isAuthenticated: true, connectionStatus: 'connected' });
    return true;
  },
});

function Swarm() {
  const { id } = useParams();
  return <output>{`swarm ${id}`}</output>;
}

function Pages({ location }: { location: Location }) {
  return useRoutes(
    [
      { path: SWARMS, element: <output>swarms</output> },
      { path: `${SWARMS}/:id`, element: <Swarm /> },
      { path: '*', element: <output>other</output> },
    ],
    location
  );
}

// The panel's shape (App.tsx, MainLayout): sign-in page, guarded pages behind a transition.
identifyInitialHistoryEntry();
const router = createHashRouter([
  {
    element: <Outlet />,
    children: [
      { path: '/login', element: <LoginPage /> },
      {
        path: '/*',
        element: (
          <ProtectedRoute>
            <PageTransition render={(location) => <Pages location={location} />} />
          </ProtectedRoute>
        ),
      },
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
const shownPage = () =>
  document.querySelector('.page-transition__layer:not([aria-hidden="true"]) output')?.textContent;
const describe = () =>
  `router ${routerPath()}, address ${addressPath()}, page ${shownPage() ?? 'none'}`;
const frame = () => new Promise((resolve) => requestAnimationFrame(resolve));

async function waitFor(read: () => unknown, label: string) {
  const deadline = performance.now() + 4000;
  while (performance.now() < deadline) {
    if (read()) return;
    await frame();
  }
  throw new Error(`${label} (${describe()})`);
}

// The router, the address and the page agree, and no page transition is under way.
const showing = (path: string, page: string) =>
  routerPath() === path &&
  addressPath() === path &&
  shownPage() === page &&
  !document.querySelector('.page-transition--animating');

let requestSeq = 0;
async function typeAddress(path: string) {
  const seq = String(++requestSeq);
  const url = `${location.href.split('#')[0]}#${path}`;
  document.body.dataset.request = JSON.stringify({ seq, url });
  await waitFor(() => document.body.dataset.done === seq, `browser did not type ${path}`);
}

const scenarios: Record<string, () => Promise<void>> = {
  // The issue as filed: the swarm page of a signed-in panel, then the swarm list typed.
  'signed-in': async () => {
    await waitFor(() => showing(SWARM, 'swarm s-docs'), 'swarm page did not open');
    await typeAddress(SWARMS);
  },
  // Typed while the session is still being restored on the sign-in page.
  restoring: async () => {
    await waitFor(() => routerPath() === '/login', 'session restore did not start');
    await typeAddress(SWARMS);
    finishRestore();
  },
  // Typed as soon as the session is restored, as the audit did after a reload.
  restored: async () => {
    await waitFor(() => routerPath() === '/login', 'session restore did not start');
    finishRestore();
    await waitFor(() => useAuthStore.getState().isAuthenticated, 'session was not restored');
    await typeAddress(SWARMS);
  },
};

async function run() {
  const scenario = scenarios[scenarioName];
  if (!scenario) throw new Error(`unknown scenario ${scenarioName}`);
  if (loads > 1) throw new Error('page reloaded');
  // Until the document is completely loaded a hash change replaces the entry instead of adding one.
  if (document.readyState !== 'complete') {
    await new Promise((resolve) => addEventListener('load', resolve, { once: true }));
  }
  await scenario();
  await waitFor(() => showing(SWARMS, 'swarms'), 'typed address did not open its page');
  // Nothing may take the panel away afterwards; the restore of the audited revision did so
  // 1.5 s after the session answered.
  const until = performance.now() + 2000;
  while (performance.now() < until) {
    if (!showing(SWARMS, 'swarms')) throw new Error(`typed address was replaced (${describe()})`);
    await frame();
  }
  return 'opened';
}

void run().then(
  (outcome) => {
    document.body.dataset.result = `PASS ${scenarioName}: ${outcome}`;
  },
  (error: Error) => {
    document.body.dataset.result = `FAIL ${scenarioName}: ${error.message}`;
  }
);
