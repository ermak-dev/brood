// Browser fixture: настоящий маршрут и decoder, только synthetic GET, без Go API/акторов.
import { createRoot } from 'react-dom/client';
import { createMemoryRouter, RouterProvider } from 'react-router-dom';
import i18n from '@/i18n';
import { SwarmPassTracePage } from '@/features/swarms/SwarmPassTrace';
import '@/styles/global.scss';
import { childTrace, ephemeralTrace } from './passTrace';

await i18n.changeLanguage('en');
const scenario = new URLSearchParams(location.search).get('scenario');
const value =
  scenario === 'ephemeral'
    ? ephemeralTrace()
    : childTrace(scenario === 'unprepared' || scenario === 'legacy' ? [] : undefined);
if (scenario === 'legacy') delete value.steps[0].childInstructions;
window.fetch = async (input, init) => {
  if (String(input) === '/api/swarms/swarm/passes/1' && init?.method === 'GET') {
    return new Response(JSON.stringify(value), { status: 200 });
  }
  throw new Error('Unexpected request in isolated child trace fixture');
};
const router = createMemoryRouter(
  [{ path: '/swarms/:id/passes/:pass', element: <SwarmPassTracePage /> }],
  { initialEntries: ['/swarms/swarm/passes/1'] }
);
createRoot(document.getElementById('root')!).render(<RouterProvider router={router} />);
