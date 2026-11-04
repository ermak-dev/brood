// FEATURE: #163: direct PTY client works in the unchanged opaque-origin sandbox.
// SPEC: Phone fit, ordinary input, native scrollback, no tmux in any control surface.
// IMPLEMENTATION: Real Chromium touch gestures and native xterm normal/alternate buffers.
// RESULT: IMPLEMENTED; opaque origin, phone fit, fixed choice, keys and touch mouse reports pass.
// STATUS: DONE
// Локальный ttyd protocol fake: это проверка клиента, не живого гостевого runtime.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const {chromium, devices} = require(process.argv[3]);
const html = fs.readFileSync(process.argv[2], 'utf8');
(async () => {
  const server = http.createServer((req, res) => {
    if (req.url.endsWith('/token')) {
      res.setHeader('Content-Type', 'application/json');
      res.setHeader('Access-Control-Allow-Origin', '*');
      res.end('{"token":""}');
      return;
    }
    res.setHeader('Content-Type', 'text/html');
    res.setHeader('Content-Security-Policy', "sandbox allow-scripts; default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data: blob:; connect-src 'self'; font-src data:");
    res.end(html);
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const browser = await chromium.launch({headless: true});
  try {
    const page = await browser.newPage({...devices['iPhone 13'], viewport: {width: 390, height: 844}});
    const errors = [];
    const input = [];
    let socket;
    let initialSize;
    page.on('pageerror', error => errors.push(error.message));
    await page.routeWebSocket('**/ws**', ws => {
      socket = ws;
      ws.onMessage(data => {
        const text = Buffer.from(data).toString();
        if (text.startsWith('{')) initialSize = JSON.parse(text);
        else if (text.startsWith('0')) input.push(text.slice(1));
      });
    });
    await page.goto(`http://127.0.0.1:${server.address().port}/actor/`);
    await page.locator('.xterm-screen').waitFor();
    await page.waitForFunction(() => document.querySelector('.terminal-quickbar'));
    assert.equal(await page.evaluate(() => window.origin), 'null');
    assert.equal(await page.getByRole('button', {name: 'tmux', exact: true}).count(), 0);
    await page.getByRole('button', {name: 'More', exact: true}).click();
    assert.equal(await page.locator('input[name="terminal-viewport-size"][value="auto"]').isChecked(), true);
    assert.equal(await page.locator('.terminal-menu').getByText(/tmux/i).count(), 0);
    assert.equal(await page.locator('.terminal-menu').getByRole('button', {name: 'Prefix', exact: true}).count(), 0);
    await page.locator('input[name="terminal-viewport-size"][value="80x60"]').check();
    assert.equal(await page.locator('input[name="terminal-viewport-size"][value="80x60"]').isChecked(), true);
    await page.setViewportSize({width: 430, height: 900});
    assert.equal(await page.locator('input[name="terminal-viewport-size"][value="80x60"]').isChecked(), true);
    await page.locator('input[name="terminal-viewport-size"][value="auto"]').check();
    await page.getByRole('button', {name: 'Close terminal menu', exact: true}).click();
    await page.getByRole('button', {name: 'Send Enter to terminal', exact: true}).click();
    await page.getByRole('button', {name: 'Send ↑ to terminal', exact: true}).click();
    await page.getByRole('button', {name: 'Open full terminal keyboard'}).click();
    assert.equal(await page.locator('.keyboard-overlay').getByText(/tmux/i).count(), 0);
    await page.locator('.keyboard-overlay').getByRole('button', {name: 'a', exact: true}).click();
    await page.getByRole('button', {name: 'Close keyboard', exact: true}).click();
    await page.waitForFunction(() => document.querySelector('.xterm-screen'));
    assert.ok(initialSize.columns < 80, JSON.stringify(initialSize));
    assert.ok(input.includes('\r'), JSON.stringify(input));
    assert.ok(input.includes('\x1b[A'), JSON.stringify(input));
    assert.ok(input.includes('a'), JSON.stringify(input));
    assert.ok(input.every(bytes => !bytes.includes('\x02')), JSON.stringify(input));
    socket.send(Buffer.from('0' + Array.from({length: 180}, (_, i) => `native-line-${i}\r\n`).join('')));
    await page.waitForFunction(() => window.term.buffer.active.baseY > 0);
    const before = await page.evaluate(() => window.term.buffer.active.viewportY);
    await page.locator('.xterm-screen').hover({position: {x: 20, y: 100}});
    await page.mouse.wheel(0, -500);
    await page.waitForFunction(before => window.term.buffer.active.viewportY < before, before);
    await page.evaluate(() => window.term.scrollToBottom());
    await page.waitForFunction(() => window.term.buffer.active.viewportY === window.term.buffer.active.baseY);
    const touchBefore = await page.evaluate(() => window.term.buffer.active.viewportY);
    const cdp = await page.context().newCDPSession(page);
    await cdp.send('Input.dispatchTouchEvent', {type: 'touchStart', touchPoints: [{x: 100, y: 120}]});
    for (const y of [150, 190, 240, 300, 360]) {
      await cdp.send('Input.dispatchTouchEvent', {type: 'touchMove', touchPoints: [{x: 100, y}]});
    }
    await cdp.send('Input.dispatchTouchEvent', {type: 'touchEnd', touchPoints: []});
    await page.waitForFunction(before => window.term.buffer.active.viewportY < before, touchBefore);
    await page.evaluate(() => {
      window.nativeTestInput = [];
      window.term.onData(bytes => window.nativeTestInput.push(bytes));
    });
    socket.send(Buffer.from('0\x1b[?1049h\x1b[?1000h\x1b[?1006h\x1b[Hnative-tui'));
    await page.waitForFunction(() => window.term.buffer.active.type === 'alternate' && window.term.buffer.active.getLine(0).translateToString().startsWith('native-tui'));
    await page.evaluate(() => { window.nativeTestInput = []; });
    await cdp.send('Input.dispatchTouchEvent', {type: 'touchStart', touchPoints: [{x: 100, y: 360}]});
    for (const y of [330, 290, 240, 180, 120]) {
      await cdp.send('Input.dispatchTouchEvent', {type: 'touchMove', touchPoints: [{x: 100, y}]});
    }
    await cdp.send('Input.dispatchTouchEvent', {type: 'touchEnd', touchPoints: []});
    await page.waitForFunction(() => window.nativeTestInput.some(bytes => /^\x1b\[<65;/.test(bytes)));
    assert.ok(input.every(bytes => !bytes.includes('\x02')), JSON.stringify(input));
    assert.equal(errors.length, 0, JSON.stringify(errors));
    console.log(JSON.stringify({origin: 'null', initialSize, input, nativeScroll: true, touchScroll: true, alternateScreenMouse: true, errors}));
  } finally {
    await browser.close();
    await new Promise(resolve => server.close(resolve));
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
