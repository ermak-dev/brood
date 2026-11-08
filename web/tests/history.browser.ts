// Real-browser regression: bun run test:browser (requires Google Chrome).
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const profile = await mkdtemp(join(tmpdir(), 'brood-history-'));
const reserve = Bun.serve({ port: 0, fetch: () => new Response() });
const port = reserve.port;
reserve.stop();
const url = `http://127.0.0.1:${port}`;
const vite = Bun.spawn(['./node_modules/.bin/vite', '--host', '127.0.0.1', '--port', String(port), '--strictPort'], { stdout: 'ignore', stderr: 'inherit' });
let chrome: ReturnType<typeof Bun.spawn> | undefined;
const pause = () => Bun.sleep(50);
try {
  chrome = Bun.spawn([process.env.CHROME_BIN || 'google-chrome', '--headless', '--disable-gpu', '--remote-debugging-port=0', `--user-data-dir=${profile}`, 'about:blank'], { stdout: 'ignore', stderr: 'ignore' });
  const deadline = Date.now() + 15000;
  let debugPort = '';
  let ready = false;
  while (Date.now() < deadline && (!debugPort || !ready)) {
    try { debugPort = (await readFile(join(profile, 'DevToolsActivePort'), 'utf8')).split('\n')[0]; } catch { /* Chrome is starting. */ }
    try { ready = (await fetch(`${url}/tests/fixtures/history-e2e.html`)).ok; } catch { /* Vite is starting. */ }
    if (!debugPort || !ready) await pause();
  }
  if (!debugPort || !ready) throw new Error('Vite or Chrome did not start within 15 seconds');
  const target = await fetch(`http://127.0.0.1:${debugPort}/json/new?${encodeURIComponent(`${url}/tests/fixtures/history-e2e.html?run`)}`, { method: 'PUT' }).then((response) => response.json()) as { webSocketDebuggerUrl: string };
  const socket = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise<void>((resolve, reject) => { socket.onopen = () => resolve(); socket.onerror = () => reject(new Error('Chrome DevTools connection failed')); });
  let id = 0;
  const evaluate = () => new Promise<string | undefined>((resolve, reject) => {
    const requestId = ++id;
    const timer = setTimeout(() => reject(new Error('Chrome DevTools response timed out')), 5000);
    const listener = (event: MessageEvent) => {
      const message = JSON.parse(String(event.data));
      if (message.id !== requestId) return;
      clearTimeout(timer);
      socket.removeEventListener('message', listener);
      if (message.error) reject(new Error(message.error.message));
      else resolve(message.result.result.value);
    };
    socket.addEventListener('message', listener);
    socket.send(JSON.stringify({ id: requestId, method: 'Runtime.evaluate', params: { expression: 'document.body && document.body.dataset.result', returnByValue: true } }));
  });
  let result: string | undefined;
  while (Date.now() < deadline && !result) { result = await evaluate(); if (!result) await pause(); }
  socket.close();
  if (result !== 'PASS back=250 forward=500 replace=500 tall=1200 direct=250') throw new Error(result || 'Browser regression timed out');
  console.log(result);
} finally {
  vite.kill();
  chrome?.kill();
  await Promise.all([vite.exited, chrome?.exited]);
  await rm(profile, { recursive: true, force: true });
}
