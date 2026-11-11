// FEATURE: Addressable selection of a swarm and an object in the URL (#194, epic #190).
// SPEC: /actors?project=P&swarm=S&actor=A opens project P, filters the page by
// swarm S and opens the panel of actor A; templates, workers and pools follow
// the same scheme. Closing the panel removes the parameter: back to the list it
// was opened from, or the same address without it for a link. Every object
// type has one address builder that the shared link component uses.
import { describe, expect, test } from 'bun:test';
import {
  CLOSED_PANEL,
  PANEL_ENTRY,
  closeTarget,
  followPanel,
  objectPath,
  readSelection,
  selectedIn,
  swarmsShown,
  withSelection,
  type ObjectRef,
} from '@/features/objects/address';
import { projectSelectionPath } from '@/features/access/shardState';

describe('reading a selection', () => {
  test('known parameters are read, unknown ones are not selection', () => {
    expect(readSelection('?project=p&swarm=s&actor=a&probe=1')).toEqual({
      project: 'p',
      swarm: 's',
      actor: 'a',
    });
    expect(readSelection('template=t&worker=w&pool=n&proxmox=x')).toEqual({
      template: 't',
      worker: 'w',
      pool: 'n',
      proxmox: 'x',
    });
  });

  test('an empty value selects nothing and the first repeat wins', () => {
    expect(readSelection('')).toEqual({});
    expect(readSelection('?')).toEqual({});
    expect(readSelection('?project=&swarm=s')).toEqual({ swarm: 's' });
    expect(readSelection('?actor=first&actor=second')).toEqual({ actor: 'first' });
  });

  test('values are decoded', () => {
    expect(readSelection('?actor=a%2Fb%20c&template=x+y&swarm=%D1%80%D0%BE%D0%B9')).toEqual({
      actor: 'a/b c',
      template: 'x y',
      swarm: 'рой',
    });
  });
});

describe('changing a selection', () => {
  test('a new parameter is added, other parameters keep their order', () => {
    expect(withSelection('?probe=1', { project: 'p', swarm: 's', actor: 'a' })).toBe(
      '?probe=1&project=p&swarm=s&actor=a'
    );
    expect(withSelection('', { worker: 'w' })).toBe('?worker=w');
  });

  test('new parameters follow the address order, not the order of the change', () => {
    expect(withSelection('', { actor: 'a', swarm: 's', project: 'p' })).toBe(
      '?project=p&swarm=s&actor=a'
    );
  });

  test('a known parameter is replaced in place and its repeats are dropped', () => {
    expect(withSelection('?swarm=old&probe=1&swarm=again', { swarm: 'new' })).toBe(
      '?swarm=new&probe=1'
    );
  });

  test('null removes a parameter; an empty result has no question mark', () => {
    expect(withSelection('?project=p&swarm=s&actor=a', { actor: null })).toBe('?project=p&swarm=s');
    expect(withSelection('?actor=a', { actor: null })).toBe('');
    expect(withSelection('?probe=1', { actor: null })).toBe('?probe=1');
  });

  test('an empty string removes a parameter as null does', () => {
    expect(withSelection('?project=p&swarm=s', { swarm: '' })).toBe('?project=p');
  });

  test('values that would break the query are encoded and read back', () => {
    const search = withSelection('', { project: 'p&q', swarm: 'a=b', actor: 'x#y z/w' });
    expect(search).not.toContain('#');
    expect(readSelection(search)).toEqual({ project: 'p&q', swarm: 'a=b', actor: 'x#y z/w' });
  });
});

describe('object addresses', () => {
  test('each object type has its address', () => {
    const cases: Array<[ObjectRef, string]> = [
      [{ kind: 'swarm', id: 's-1' }, '/swarms/s-1'],
      // Прогон -- это рой (#206): ссылка на прогон ведёт на страницу роя.
      [{ kind: 'run', swarm: 's-1' }, '/swarms/s-1'],
      [{ kind: 'actor', project: 'p', swarm: 's', name: 'a' }, '/actors?project=p&swarm=s&actor=a'],
      [
        { kind: 'template', project: 'p', swarm: 's', name: 't' },
        '/templates?project=p&swarm=s&template=t',
      ],
      [{ kind: 'worker', name: 'w' }, '/workers?worker=w'],
      [{ kind: 'pool', name: 'n' }, '/worker-pools?pool=n'],
      [{ kind: 'proxmoxPool', name: 'x' }, '/worker-pools?proxmox=x'],
    ];
    for (const [ref, path] of cases) expect([ref.kind, objectPath(ref)]).toEqual([ref.kind, path]);
  });

  test('without a name the address is the list of the swarm', () => {
    expect(objectPath({ kind: 'actor', project: 'p', swarm: 's' })).toBe(
      '/actors?project=p&swarm=s'
    );
    expect(objectPath({ kind: 'template', project: 'p', swarm: 's' })).toBe(
      '/templates?project=p&swarm=s'
    );
  });

  test('a swarm ID is one path segment', () => {
    expect(objectPath({ kind: 'swarm', id: 'a/b c' })).toBe('/swarms/a%2Fb%20c');
    expect(objectPath({ kind: 'run', swarm: 'x?y' })).toBe('/swarms/x%3Fy');
  });

  test('the page reads back exactly the object the address names', () => {
    const path = objectPath({ kind: 'actor', project: 'p/1', swarm: 's&2', name: 'a b' });
    expect(readSelection(path.slice(path.indexOf('?')))).toEqual({
      project: 'p/1',
      swarm: 's&2',
      actor: 'a b',
    });
  });
});

describe('the object of a swarm section', () => {
  test('an actor or template is selected only in the swarm the address names', () => {
    const selection = readSelection('?swarm=s&actor=a&template=t');
    expect(selectedIn(selection, 's', 'actor')).toBe('a');
    expect(selectedIn(selection, 's', 'template')).toBe('t');
    expect(selectedIn(selection, 'other', 'actor')).toBeUndefined();
  });

  test('without a swarm a name does not open a panel: names repeat across swarms', () => {
    expect(selectedIn(readSelection('?actor=a'), 's', 'actor')).toBeUndefined();
  });
});

describe('the swarms a page shows', () => {
  const swarms = [{ id: 's1' }, { id: 's2' }];

  test('without a swarm in the address every swarm is shown', () => {
    expect(swarmsShown(swarms, undefined)).toEqual({ shown: swarms, missing: false });
  });

  test('the swarm of the address is the only one shown', () => {
    expect(swarmsShown(swarms, 's2')).toEqual({ shown: [swarms[1]], missing: false });
  });

  test('a swarm that is not available is named missing, not replaced by all', () => {
    expect(swarmsShown(swarms, 'gone')).toEqual({ shown: [], missing: true });
  });
});

describe('closing a panel', () => {
  test('a panel opened from the list goes back to that entry', () => {
    expect(closeTarget(PANEL_ENTRY, '?project=p&swarm=s&actor=a', 'actor')).toEqual({
      back: true,
    });
  });

  test('a panel opened by a link stays on the page without the parameter', () => {
    for (const state of [null, undefined, {}, { from: 'elsewhere' }, 'panel']) {
      expect(closeTarget(state, '?project=p&swarm=s&actor=a', 'actor')).toEqual({
        search: '?project=p&swarm=s',
      });
    }
    expect(closeTarget(null, '?worker=w&probe=1', 'worker')).toEqual({ search: '?probe=1' });
  });
});

describe('a panel follows the address', () => {
  test('nothing selected keeps a closed panel as it is', () => {
    expect(followPanel(CLOSED_PANEL, undefined)).toBe(CLOSED_PANEL);
  });

  test('a selected object opens a new showing of the panel', () => {
    expect(followPanel(CLOSED_PANEL, 'a')).toEqual({ name: 'a', open: true, session: 1 });
  });

  test('the same object keeps the open panel as it is', () => {
    const open = { name: 'a', open: true, session: 1 };
    expect(followPanel(open, 'a')).toBe(open);
  });

  test('closing keeps the name while the panel slides away', () => {
    expect(followPanel({ name: 'a', open: true, session: 1 }, undefined)).toEqual({
      name: 'a',
      open: false,
      session: 1,
    });
  });

  test('another object or a reopen starts a new showing', () => {
    expect(followPanel({ name: 'a', open: true, session: 1 }, 'b')).toEqual({
      name: 'b',
      open: true,
      session: 2,
    });
    expect(followPanel({ name: 'a', open: false, session: 1 }, 'a')).toEqual({
      name: 'a',
      open: true,
      session: 2,
    });
  });
});

describe('switching the project in the header', () => {
  test('the selection of the old project leaves the address', () => {
    expect(projectSelectionPath('/actors', 'new', '?project=old&swarm=s&actor=a&probe=1')).toBe(
      '/actors?probe=1'
    );
    expect(projectSelectionPath('/templates', 'new', '?swarm=s&template=t')).toBe('/templates');
  });

  test('an address without a selection stays as it is', () => {
    for (const path of ['/actors', '/templates', '/swarms', '/projects']) {
      expect(projectSelectionPath(path, 'new', '')).toBeNull();
      expect(projectSelectionPath(path, 'new', '?probe=1')).toBeNull();
    }
  });

  test('installation objects do not belong to a project', () => {
    expect(projectSelectionPath('/workers', 'new', '?worker=w')).toBeNull();
    expect(projectSelectionPath('/worker-pools', 'new', '?pool=n')).toBeNull();
  });

  test('detail and swarm pages keep their former destinations', () => {
    expect(projectSelectionPath('/projects/old', 'new/id', '?project=old')).toBe(
      '/projects/new%2Fid'
    );
    expect(projectSelectionPath('/swarms/s-1', 'new', '?probe=1')).toBe('/swarms');
  });
});
