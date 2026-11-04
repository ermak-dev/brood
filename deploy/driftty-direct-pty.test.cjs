// FEATURE: #163: direct PTY controls and responsive first-visit viewport.
// SPEC: No multiplexer modes/actions; normal keys and explicit sizes survive.
// IMPLEMENTATION: Exact source adaptation, with upstream typecheck and tests retained.
// RESULT: IMPLEMENTED; all four behaviors fail on upstream and pass after adaptation.
// STATUS: DONE
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const {createRequire} = require('node:module');
const {test, after} = require('node:test');
const {pathToFileURL} = require('node:url');
const root = path.resolve(process.argv[2]);
const ts = process.argv[3] ? require(process.argv[3]) : undefined;
let server;
const ready = ts ? Promise.resolve() : (async () => {
  const requireFromBuild = createRequire(path.join(root, 'package.json'));
  const {createServer} = await import(pathToFileURL(requireFromBuild.resolve('vite')).href);
  server = await createServer({configFile: false, root, server: {middlewareMode: true}});
})();
after(async () => { if (server) await server.close(); });
async function moduleAt(relative) {
  await ready;
  return ts ? load(relative) : server.ssrLoadModule('/' + relative);
}
const cache = new Map();
function load(relative) {
  const filename = path.resolve(root, relative);
  if (cache.has(filename)) return cache.get(filename);
  const module = {exports: {}};
  cache.set(filename, module.exports);
  const source = ts.transpileModule(fs.readFileSync(filename, 'utf8'), {
    compilerOptions: {module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2020},
  }).outputText;
  vm.runInThisContext(`(function(require,module,exports){${source}\n})`, {filename})(
    (name) => load(path.relative(root, path.resolve(path.dirname(filename), name + '.ts'))),
    module, module.exports,
  );
  return module.exports;
}
const component = 'src/components/';
test('available modes reject old multiplexer preference in both keyboards', async () => {
  const modes = await moduleAt(component + 'terminal-quickbar/mode.ts');
  assert.deepEqual(modes.quickbarModes, ['agent', 'nav', 'ctrl', 'copy']);
  assert.deepEqual(modes.keyboardQuickbarModes, ['agent', 'nav', 'ctrl']);
  assert.equal(modes.loadQuickbarMode({getItem: () => 'tmux'}), 'agent');
});
test('every exposed quickbar action sends ordinary terminal bytes', async () => {
  const {quickbarControls, quickbarControlSequence} = await moduleAt(component + 'terminal-quickbar/controls.ts');
  assert.deepEqual(Object.keys(quickbarControls), ['agent', 'nav', 'ctrl', 'copy']);
  for (const controls of Object.values(quickbarControls)) {
    for (const control of controls) {
      const bytes = quickbarControlSequence(control);
      assert.equal(typeof bytes, 'string');
      assert.ok(!bytes.includes('\x02'), `${control.label} injects a multiplexer prefix`);
    }
  }
  const {terminalActionSequence, controlSequence} = await moduleAt(component + 'terminal-actions.ts');
  for (const [action, bytes] of Object.entries({escape: '\x1b', up: '\x1b[A', 'page-up': '\x1b[5~', 'page-down': '\x1b[6~', enter: '\r', slash: '/', tab: '\t'})) {
    assert.equal(terminalActionSequence(action), bytes);
  }
  assert.equal(controlSequence('C'), '\x03');
  assert.equal(controlSequence('B'), '\x02'); // Ordinary Ctrl-B is not prohibited.
});
test('first visit fits phone and desktop; stored fixed sizes remain explicit choices', async () => {
  const {FixedMobileViewport} = await moduleAt(component + 'terminal/fixed-mobile-viewport.ts');
  for (const [stored, expected] of [[null, 'auto'], ['garbage', 'auto'], ['auto', 'auto'], ['80x60', '80x60'], ['custom:92x33', 'custom:92x33'], ['80x24', 'custom:80x24']]) {
    for (const viewportSize of [{width: 390, height: 844}, {width: 1440, height: 900}]) {
      const viewport = new FixedMobileViewport({
        mobile: true, viewportSize,
        storage: {getItem: () => stored, setItem: () => {}},
        terminal: {setFixedSize() {}, cellSize() {}, cancelTouchSelection() {}, cancelTouchScroll() {}},
        viewport: () => undefined, onChange() {}, schedule() {},
      });
      assert.equal(viewport.view.size, expected, JSON.stringify({stored, viewportSize}));
    }
  }
});
test('menu, keyboard and terminal have no multiplexer action paths or scroll mode', async () => {
  for (const file of ['terminal-menu/index.tsx', 'terminal-quickbar/index.tsx', 'keyboard-overlay/index.tsx', 'terminal/index.tsx', 'terminal-actions.ts', 'keyboard-overlay/keys.ts']) {
    const source = fs.readFileSync(path.join(root, component, file), 'utf8');
    assert.doesNotMatch(source, /tmux|scrollControls/);
  }
});
