// BUG: Same-path history entries lose their individual scroll positions.
// HYPOTHESIS: Updating a layer in place skips saving and restoring entry scroll.
// SOLUTION: Restore scroll after rendering and preserve it on replace.
// RESULT: Router-keyed back, forward, replace, and variable-height content pass.
// STATUS: PARTIAL — direct-entry restoration regression is under test.
// BUG: Returning to a directly opened page loses its scroll position.
// HYPOTHESIS: Default-key history entries are discarded instead of identified.
import { useRef } from 'react';
import { createRoot } from 'react-dom/client';
import { HashRouter, useNavigate } from 'react-router-dom';
import { PageTransition } from '../../src/components/common/PageTransition';
import { identifyInitialHistoryEntry } from '../../src/router/initialHistoryEntry';

Object.defineProperty(window.crypto, 'randomUUID', { value: undefined, configurable: true });
identifyInitialHistoryEntry();

function Fixture() {
  const scrollRef = useRef<HTMLDivElement>(null);
  const navigate = useNavigate();
  return <>
    <button onClick={() => navigate('/page?entry=one', { state: { entry: 'one' } })}>One</button>
    <button onClick={() => navigate('/page?entry=two', { state: { entry: 'two' } })}>Two</button>
    <button onClick={() => navigate('/other')}>Other</button>
    <button onClick={() => navigate('/page?entry=replaced', { replace: true })}>Replace</button>
    <div ref={scrollRef} id="scroll" style={{ height: 200, overflow: 'auto' }}>
      <PageTransition scrollContainerRef={scrollRef} render={(location) => <div style={{ height: location.search === '?entry=one' ? 2000 : 800 }}>
        <output>{JSON.stringify({ path: location.pathname, search: location.search, state: location.state })}</output>
      </div>} />
    </div>
  </>;
}
createRoot(document.getElementById('root')!).render(<HashRouter><Fixture /></HashRouter>);

// Poll the rendered state, not a fixed animation duration; fail rather than hang.
async function waitForEntry(search: string, path?: string) {
  const deadline = performance.now() + 5000;
  while (performance.now() < deadline) {
    const current = document.querySelector('.page-transition__layer:not([aria-hidden="true"]) output');
    if (current && JSON.parse(current.textContent!).search === search &&
        (!path || JSON.parse(current.textContent!).path === path) &&
        !document.querySelector('.page-transition--animating')) return;
    await new Promise((resolve) => requestAnimationFrame(resolve));
  }
  throw new Error(`Timed out waiting for ${search}`);
}

async function testHistoryScroll() {
  await waitForEntry('');
  const [one, two] = document.querySelectorAll('button');
  const scroll = document.getElementById('scroll')!;
  scroll.scrollTop = 250;
  document.querySelectorAll('button')[2].click();
  await waitForEntry('', '/other');
  history.back();
  await waitForEntry('', '/');
  if (scroll.scrollTop !== 250) throw new Error(`direct back scroll: expected 250, got ${scroll.scrollTop}`);
  one.click();
  await waitForEntry('?entry=one');
  scroll.scrollTop = 250;
  two.click();
  await waitForEntry('?entry=two');
  scroll.scrollTop = 500;
  history.back();
  await waitForEntry('?entry=one');
  if (scroll.scrollTop !== 250) throw new Error(`back scroll: expected 250, got ${scroll.scrollTop}`);
  history.forward();
  await waitForEntry('?entry=two');
  if (scroll.scrollTop !== 500) throw new Error(`forward scroll: expected 500, got ${scroll.scrollTop}`);
  document.querySelectorAll('button')[3].click();
  await waitForEntry('?entry=replaced');
  if (scroll.scrollTop !== 500) throw new Error(`replace scroll: expected 500, got ${scroll.scrollTop}`);
  history.back();
  await waitForEntry('?entry=one');
  scroll.scrollTop = 1200;
  history.forward();
  await waitForEntry('?entry=replaced');
  history.back();
  await waitForEntry('?entry=one');
  if (scroll.scrollTop !== 1200) throw new Error(`tall page scroll: expected 1200, got ${scroll.scrollTop}`);
  location.hash = '#/direct';
  await waitForEntry('', '/direct');
  scroll.scrollTop = 250;
  document.querySelectorAll('button')[2].click();
  await waitForEntry('', '/other');
  history.back();
  await waitForEntry('', '/direct');
  if (scroll.scrollTop !== 250) throw new Error(`direct hash back scroll: expected 250, got ${scroll.scrollTop}`);
  return 'back=250 forward=500 replace=500 tall=1200 direct=250';
}

Object.assign(window, { testHistoryScroll });
if (new URLSearchParams(location.search).has('run')) {
  void testHistoryScroll().then(
    (result) => { document.body.dataset.result = `PASS ${result}`; },
    (error: Error) => { document.body.dataset.result = `FAIL ${error.message}`; }
  );
}
