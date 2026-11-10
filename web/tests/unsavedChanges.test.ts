// BUG: A direct hash entry has no router index; Back past the blocker loses unsaved edits (#202).
// HYPOTHESIS: The router cancels a Back/Forward by moving back the index difference, which is
// absent (no prompt), NaN (history.go(NaN) reloads) or wrong (lands on another entry).
// SOLUTION: Cancel only traversals between router-indexed entries; otherwise let the transition
// happen and keep the unsaved draft outside the editor so reopening it offers the edit back.
// STATUS: FIXED
import { describe, expect, test } from 'bun:test';
import { parse } from 'yaml';
import {
  getModelAliasDraftSignature,
  getStringSetSignature,
  parseExcludedModelsDraft,
  parseModelAliasDraft,
} from '@/features/authFiles/oauthEditorState';
import { parseConfigDraft, resolveConfigDraft } from '@/features/config/unsavedDraft';
import { canCancelTraversal, readHistoryIndex } from '@/router/initialHistoryEntry';
import { createDraftStore } from '@/services/storage/unsavedDrafts';

describe('router history index', () => {
  test('only an integer idx written by the router counts', () => {
    expect(readHistoryIndex({ usr: null, key: 'a', idx: 3 })).toBe(3);
    expect(readHistoryIndex({ idx: 0 })).toBe(0);
    // A push from an entry without idx stores undefined + 1.
    expect(readHistoryIndex({ idx: Number.NaN })).toBeUndefined();
    expect(readHistoryIndex({ key: 'external' })).toBeUndefined();
    expect(readHistoryIndex(null)).toBeUndefined();
    expect(readHistoryIndex({ idx: '1' })).toBeUndefined();
    expect(readHistoryIndex({ idx: 1.5 })).toBeUndefined();
    expect(readHistoryIndex('state')).toBeUndefined();
  });

  test('a traversal is cancellable only between distinct router indexes', () => {
    expect(canCancelTraversal(1, 0, false)).toBe(true);
    expect(canCancelTraversal(0, 2, false)).toBe(true);
    // No index on either side: the router cannot move back at all.
    expect(canCancelTraversal(undefined, 0, false)).toBe(false);
    expect(canCancelTraversal(1, undefined, false)).toBe(false);
    // A zero difference is history.go(0), a reload.
    expect(canCancelTraversal(0, 0, false)).toBe(false);
    // An unidentified entry may hold a guessed index, so differences are not distances.
    expect(canCancelTraversal(1, 0, true)).toBe(false);
  });
});

class MemoryStorage {
  readonly items = new Map<string, string>();
  get length() {
    return this.items.size;
  }
  key(index: number) {
    return [...this.items.keys()][index] ?? null;
  }
  getItem(key: string) {
    return this.items.get(key) ?? null;
  }
  setItem(key: string, value: string) {
    this.items.set(key, value);
  }
  removeItem(key: string) {
    this.items.delete(key);
  }
}

describe('unsaved draft store', () => {
  test('a draft outlives the editor and is gone after discard', () => {
    const store = createDraftStore(() => new MemoryStorage());
    store.retain('editor', { alias: 'kept' }, 'memory');
    expect(store.read('editor')).toEqual({ alias: 'kept' });
    store.discard('editor');
    expect(store.read('editor')).toBeUndefined();
  });

  test('a session draft survives a reload, a memory draft never reaches storage', () => {
    const storage = new MemoryStorage();
    const before = createDraftStore(() => storage);
    before.retain('aliases', ['kept'], 'session');
    before.retain('config', 'api-keys: [secret]', 'memory');
    expect([...storage.items.values()].join()).not.toContain('secret');

    const reloaded = createDraftStore(() => storage);
    expect(reloaded.read('aliases')).toEqual(['kept']);
    expect(reloaded.read('config')).toBeUndefined();
  });

  test('switching a draft to memory removes its stored copy', () => {
    const storage = new MemoryStorage();
    const store = createDraftStore(() => storage);
    store.retain('editor', 1, 'session');
    store.retain('editor', 2, 'memory');
    expect(storage.length).toBe(0);
    expect(store.read('editor')).toBe(2);
  });

  test('clear drops every draft but leaves other storage alone', () => {
    const storage = new MemoryStorage();
    storage.setItem('unrelated', 'kept');
    const store = createDraftStore(() => storage);
    store.retain('a', 1, 'session');
    store.retain('b', 2, 'memory');
    store.clear();
    expect(store.read('a')).toBeUndefined();
    expect(store.read('b')).toBeUndefined();
    expect([...storage.items.keys()]).toEqual(['unrelated']);
  });

  test('clear starts a new generation so a closing editor cannot write into the next session', () => {
    const store = createDraftStore(() => new MemoryStorage());
    const opened = store.generation();
    store.clear();
    expect(store.generation()).not.toBe(opened);
  });

  test('denied or corrupt storage keeps the in-tab draft', () => {
    const denied = createDraftStore(() => {
      throw new DOMException('denied', 'SecurityError');
    });
    denied.retain('editor', 'kept', 'session');
    expect(denied.read('editor')).toBe('kept');
    denied.clear();
    expect(denied.read('editor')).toBeUndefined();

    const storage = new MemoryStorage();
    storage.setItem('brood.unsaved-draft:editor', '{not json');
    expect(createDraftStore(() => storage).read('editor')).toBeUndefined();
  });
});

describe('config draft recovery', () => {
  const base = 'server: {port: 8317}\n';
  const edited = 'server: {port: 9000}\n';

  test('only a well-formed draft that differs from the server is offered', () => {
    expect(parseConfigDraft({ base, yaml: edited, mode: 'visual' }, base)).toEqual({
      base,
      yaml: edited,
      mode: 'visual',
    });
    expect(parseConfigDraft({ base, yaml: base, mode: 'source' }, base)).toBeNull();
    expect(parseConfigDraft({ base, yaml: edited, mode: 'other' }, base)).toBeNull();
    expect(parseConfigDraft({ yaml: edited, mode: 'source' }, base)).toBeNull();
    expect(parseConfigDraft(undefined, base)).toBeNull();
  });

  test('an unchanged server takes the draft verbatim', () => {
    const source = '# mine\nserver: {port: 9000}\n';
    expect(resolveConfigDraft({ base, yaml: source, mode: 'source' }, base)).toEqual({
      yaml: source,
      mode: 'source',
    });
  });

  test('a changed server keeps both the draft and the concurrent values', () => {
    const latest = 'server: {port: 8317, host: localhost}\n';
    const resolved = resolveConfigDraft({ base, yaml: edited, mode: 'visual' }, latest);
    expect(resolved.mode).toBe('visual');
    expect(parse(resolved.yaml)).toEqual({ server: { port: 9000, host: 'localhost' } });
  });

  test('a conflicting draft is restored as source so the save diff shows everything', () => {
    const listBase = 'access: {api-keys: [a]}\n';
    const draft = { base: listBase, yaml: 'access: {api-keys: [b]}\n', mode: 'visual' as const };
    expect(resolveConfigDraft(draft, 'access: {api-keys: [c]}\n')).toEqual({
      yaml: draft.yaml,
      mode: 'source',
    });
  });
});

describe('OAuth editor draft recovery', () => {
  test('model aliases: entries are validated and an unchanged draft is not offered', () => {
    const baseline = getModelAliasDraftSignature([{ name: 'claude-sonnet', alias: 'baseline' }]);
    const draft = [{ name: 'claude-sonnet', alias: 'kept', fork: true, forceMapping: false }];
    expect(parseModelAliasDraft(draft, baseline)).toEqual(draft);
    expect(
      parseModelAliasDraft([{ name: 'claude-sonnet', alias: 'baseline' }], baseline)
    ).toBeNull();
    expect(parseModelAliasDraft([{ name: 'claude-sonnet', alias: 7 }], baseline)).toBeNull();
    expect(parseModelAliasDraft([{ name: 'a', alias: 'b', fork: 'yes' }], baseline)).toBeNull();
    expect(parseModelAliasDraft({ name: 'a', alias: 'b' }, baseline)).toBeNull();
  });

  test('excluded models: a rule list that differs from the baseline is offered', () => {
    const baseline = getStringSetSignature(['gpt-4']);
    expect(parseExcludedModelsDraft(['gpt-4', 'gpt-5*'], baseline)).toEqual(['gpt-4', 'gpt-5*']);
    expect(parseExcludedModelsDraft(['gpt-4'], baseline)).toBeNull();
    expect(parseExcludedModelsDraft(['gpt-4', 5], baseline)).toBeNull();
    expect(parseExcludedModelsDraft('gpt-4', baseline)).toBeNull();
  });
});
