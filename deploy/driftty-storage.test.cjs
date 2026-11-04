// BUG: Actor-view sandbox gives driftty an opaque origin and storage getters throw.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(process.argv[2], 'utf8');
const window = {};
for (const name of ['localStorage', 'sessionStorage']) {
  Object.defineProperty(window, name, { configurable: true, get() { throw new Error('SecurityError'); } });
}
vm.runInNewContext(source, { window });
for (const name of ['localStorage', 'sessionStorage']) {
  const storage = window[name];
  assert.equal(storage.getItem('missing'), null);
  storage.setItem('key', 42);
  assert.equal(storage.getItem('key'), '42');
  assert.equal(storage.length, 1);
  assert.equal(storage.key(0), 'key');
  assert.equal(storage.key(1), null);
  storage.removeItem('key');
  assert.equal(storage.length, 0);
  storage.setItem('other', 'value');
  storage.clear();
  assert.equal(storage.getItem('other'), null);
}
window.localStorage.setItem('private', 'local');
assert.equal(window.sessionStorage.getItem('private'), null);
const available = { localStorage: { existing: true }, sessionStorage: { existing: true } };
vm.runInNewContext(source, { window: available });
assert.equal(available.localStorage.existing, true);
assert.equal(available.sessionStorage.existing, true);
console.log('Opaque-origin storage fallback passed');
