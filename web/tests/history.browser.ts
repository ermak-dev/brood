// Real-browser regression: bun run test:browser (requires Google Chrome).
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const unsaved = (run: string, nav: 'on' | 'off', outcome: string) => ({
  page: `unsaved-e2e.html?run=${run}&nav=${nav}`,
  expected: `PASS ${run} nav=${nav}: ${outcome}`,
});
// Only browser-initiated transitions are cancelled through the Navigation API; without it, and
// for scripted traversals, only router-indexed entries can be cancelled. The rest navigate and
// offer the edit back when the editor reopens.
const routerCancellable = ['managed', 'forward', 'push', 'app-back'];
const cases = [
  { page: 'history-e2e.html?run', expected: 'PASS back=250 forward=500 replace=500 tall=1200 direct=250' },
  ...['issue', 'direct-editor', 'managed', 'forward', 'push', 'app-back', 'script-back', 'typed-hash', 'write-denied'].flatMap((run) => [
    unsaved(run, 'on', run === 'script-back' ? 'restored' : 'prompted'),
    unsaved(run, 'off', routerCancellable.includes(run) ? 'prompted' : 'restored'),
  ]),
  // Without edits there is nothing to confirm: a typed address leaves the editor at once.
  unsaved('typed-hash-clean', 'on', 'left'),
  unsaved('typed-hash-clean', 'off', 'left'),
  // #258: a typed address opens its page, also while the reloaded panel restores its session.
  ...['signed-in', 'restoring', 'restored'].map((run) => ({
    page: `address-e2e.html?run=${run}#/swarms/s-docs`,
    expected: `PASS ${run}: opened`,
  })),
  // #262: a reload keeps the query and hash of its address once the session is restored.
  // A sign-in without a session returns to the address as well.
  ...[
    ['reload-swarm', '/swarms/s-docs?probe=1#step-2'],
    ['reload-project', '/projects/p-brood?project=p-brood#catalog'],
    ['sign-in', '/projects/p-brood?project=p-brood#catalog'],
  ].map(([run, address]) => ({ page: `address-e2e.html?run=${run}#${address}`, expected: `PASS ${run}: opened` })),
];

// bun run test:browser <substring> runs only the matching pages.
const selected = cases.filter(({ page }) => page.includes(process.argv[2] ?? ''));

const profile = await mkdtemp(join(tmpdir(), 'brood-history-'));
const reserve = Bun.serve({ port: 0, fetch: () => new Response() });
const port = reserve.port;
reserve.stop();
const url = `http://127.0.0.1:${port}`;
const vite = Bun.spawn(['./node_modules/.bin/vite', '--host', '127.0.0.1', '--port', String(port), '--strictPort'], { stdout: 'ignore', stderr: 'inherit' });
let chrome: ReturnType<typeof Bun.spawn> | undefined;
const pause = () => Bun.sleep(50);
const failures: string[] = [];
try {
  chrome = Bun.spawn([process.env.CHROME_BIN || 'google-chrome', '--headless', '--disable-gpu', '--remote-debugging-port=0', `--user-data-dir=${profile}`, 'about:blank'], { stdout: 'ignore', stderr: 'ignore' });
  const startup = Date.now() + 15000;
  let debugPort = '';
  let ready = false;
  while (Date.now() < startup && (!debugPort || !ready)) {
    try { debugPort = (await readFile(join(profile, 'DevToolsActivePort'), 'utf8')).split('\n')[0]; } catch { /* Chrome is starting. */ }
    try { ready = (await fetch(`${url}/tests/fixtures/history-e2e.html`)).ok; } catch { /* Vite is starting. */ }
    if (!debugPort || !ready) await pause();
  }
  if (!debugPort || !ready) throw new Error('Vite or Chrome did not start within 15 seconds');
  for (const { page, expected } of selected) {
    const result = await runPage(debugPort, `${url}/tests/fixtures/${page}`);
    if (result !== expected) failures.push(`${page}: expected "${expected}", got "${result || 'timeout'}"`);
    else console.log(result);
  }
} finally {
  vite.kill();
  chrome?.kill();
  await Promise.all([vite.exited, chrome?.exited]);
  await rm(profile, { recursive: true, force: true });
}
if (failures.length) throw new Error(`Browser regressions failed:\n${failures.join('\n')}`);

// Open the page in its own tab (fresh sessionStorage) and poll body[data-result]. The tab
// starts blank and then navigates, as a user does: Chrome ignores history.back() to the first
// entry of a tab opened by /json/new?url once a traversal has been cancelled. A fixture asks
// for browser Back/Forward or a typed address through body[data-request]. Polls count as user
// gestures, as clicks and typing do: Chrome cancels a browser Back only after one.
async function runPage(debugPort: string, pageUrl: string): Promise<string | undefined> {
  const target = await fetch(`http://127.0.0.1:${debugPort}/json/new?about:blank`, { method: 'PUT' }).then((response) => response.json()) as { id: string; webSocketDebuggerUrl: string };
  const socket = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise<void>((resolve, reject) => { socket.onopen = () => resolve(); socket.onerror = () => reject(new Error('Chrome DevTools connection failed')); });
  let id = 0;
  const send = <T>(method: string, params: object = {}) => new Promise<T>((resolve, reject) => {
    const requestId = ++id;
    const timer = setTimeout(() => reject(new Error('Chrome DevTools response timed out')), 5000);
    const listener = (event: MessageEvent) => {
      const message = JSON.parse(String(event.data));
      if (message.id !== requestId) return;
      clearTimeout(timer);
      socket.removeEventListener('message', listener);
      if (message.error) reject(new Error(message.error.message));
      else resolve(message.result);
    };
    socket.addEventListener('message', listener);
    socket.send(JSON.stringify({ id: requestId, method, params }));
  });
  const evaluate = (expression: string) => send<{ result: { value?: string } }>('Runtime.evaluate', { expression, returnByValue: true, userGesture: true }).then((reply) => reply.result.value);
  const perform = async (request: { seq: string; go?: number; url?: string }) => {
    if (request.url) await send('Page.navigate', { url: request.url });
    else {
      const history = await send<{ currentIndex: number; entries: { id: number }[] }>('Page.getNavigationHistory');
      await send('Page.navigateToHistoryEntry', { entryId: history.entries[history.currentIndex + (request.go ?? 0)].id });
    }
    await evaluate(`document.body.dataset.done = ${JSON.stringify(request.seq)}`);
  };
  const deadline = Date.now() + 30000;
  let result: string | undefined;
  let performed = '';
  try {
    await send('Page.navigate', { url: pageUrl });
    // A reloading page briefly has no execution context; keep polling.
    while (Date.now() < deadline && !result) {
      const state = await evaluate('document.body && JSON.stringify([document.body.dataset.result, document.body.dataset.request])').catch(() => undefined);
      const [done, request] = state ? JSON.parse(state) as [string | undefined, string | undefined] : [];
      result = done;
      // A reload during the action is the fixture's failure to report, not the harness's.
      if (!result && request && request !== performed) { performed = request; await perform(JSON.parse(request)).catch(() => undefined); }
      if (!result) await pause();
    }
  } finally {
    socket.close();
    await fetch(`http://127.0.0.1:${debugPort}/json/close/${target.id}`).catch(() => undefined);
  }
  return result;
}
