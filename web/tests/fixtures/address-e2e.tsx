// Real-browser regression for #258: an address typed into the address bar opens its page.
// Each page load runs one scenario (?run=<name>) and starts at a swarm page, as a reload of
// that page does. The typed address comes from tests/history.browser.ts over DevTools, so the
// navigation is the browser's, as when a user types it. The audit typed it while the reloaded
// panel restored its session, and the restore then took the panel back to the address it had
// loaded with. #262: the address the panel was reloaded at keeps its query and hash once the
// session is restored, and once the user signs in when there is no session.
import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import {
  Outlet,
  RouterProvider,
  createHashRouter,
  useLocation,
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
// Without a session the sign-in form signs in at once.
let finishRestore = () => {};
const restore = new Promise<void>((resolve) => {
  finishRestore = resolve;
});
const connect = (): void => {
  useAuthStore.setState({ isAuthenticated: true, connectionStatus: 'connected' });
};
useAuthStore.setState({
  isAuthenticated: scenarioName === 'signed-in',
  managementKey: '',
  restoreSession: async () => {
    await restore;
    if (scenarioName === 'sign-in') return false;
    connect();
    return true;
  },
  login: async () => connect(),
});

// A page shows the query and hash it was opened with: the swarm and project pages read them.
function Page({ name }: { name: string }) {
  const { search, hash } = useLocation();
  return <output>{`${name}${search}${hash}`}</output>;
}

function Swarm() {
  const { id } = useParams();
  return <Page name={`swarm ${id}`} />;
}

function Project() {
  const { project } = useParams();
  return <Page name={`project ${project}`} />;
}

function Pages({ location }: { location: Location }) {
  return useRoutes(
    [
      { path: SWARMS, element: <Page name="swarms" /> },
      { path: `${SWARMS}/:id`, element: <Swarm /> },
      { path: '/projects/:project', element: <Project /> },
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

const routerPath = () => {
  const { pathname, search, hash } = router.state.location;
  return `${pathname}${search}${hash}`;
};
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

const signInPage = () => waitFor(() => routerPath() === '/login', 'session restore did not start');
const restoreNow = async () => {
  await signInPage();
  finishRestore();
};

// Types the credentials into the sign-in form and submits it with Enter, as a user does.
async function submitSignIn() {
  const field = (name: string) => document.querySelector<HTMLInputElement>(`input[name="${name}"]`);
  await waitFor(() => field('password'), 'sign-in form did not open');
  for (const [name, value] of [
    ['username', 'root'],
    ['password', 'secret'],
  ]) {
    const input = field(name)!;
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!.call(input, value);
    input.dispatchEvent(new Event('input', { bubbles: true }));
  }
  await frame();
  field('password')!.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
}

// Each scenario ends at its address with its page, and stays there.
const scenarios: Record<string, { act: () => Promise<void>; address: string; page: string }> = {
  // The issue as filed: the swarm page of a signed-in panel, then the swarm list typed.
  'signed-in': {
    address: SWARMS,
    page: 'swarms',
    act: async () => {
      await waitFor(() => showing(SWARM, 'swarm s-docs'), 'swarm page did not open');
      await typeAddress(SWARMS);
    },
  },
  // Typed while the session is still being restored on the sign-in page.
  restoring: {
    address: SWARMS,
    page: 'swarms',
    act: async () => {
      await signInPage();
      await typeAddress(SWARMS);
      finishRestore();
    },
  },
  // Typed as soon as the session is restored, as the audit did after a reload.
  restored: {
    address: SWARMS,
    page: 'swarms',
    act: async () => {
      await restoreNow();
      await waitFor(() => useAuthStore.getState().isAuthenticated, 'session was not restored');
      await typeAddress(SWARMS);
    },
  },
  // #262: the page reloaded at an address with a query and a hash opens that address again.
  'reload-swarm': {
    address: `${SWARM}?probe=1#step-2`,
    page: 'swarm s-docs?probe=1#step-2',
    act: restoreNow,
  },
  'reload-project': {
    address: '/projects/p-brood?project=p-brood#catalog',
    page: 'project p-brood?project=p-brood#catalog',
    act: restoreNow,
  },
  // Without a session the form signs in and opens the address too: the #240 audit saw a fresh
  // sign-in at #/projects/p-brood open another page.
  'sign-in': {
    address: '/projects/p-brood?project=p-brood#catalog',
    page: 'project p-brood?project=p-brood#catalog',
    act: async () => {
      await restoreNow();
      await submitSignIn();
    },
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
  const { act, address, page } = scenario;
  await act();
  await waitFor(() => showing(address, page), `${address} did not open its page`);
  // Nothing may take the panel away afterwards; the restore of the audited revision did so
  // 1.5 s after the session answered.
  const until = performance.now() + 2000;
  while (performance.now() < until) {
    if (!showing(address, page)) throw new Error(`${address} was replaced (${describe()})`);
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
