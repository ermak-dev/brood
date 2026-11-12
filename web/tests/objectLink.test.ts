// FEATURE: One link component for every object type (#194, epic #190).
// SPEC: A swarm, run, actor, template, worker, pool or workflow link leads to the
// object's address from objectPath; without children it is labelled by the
// object's name. #195 and #197 reuse it for relations and the project overview.
import { describe, expect, test } from 'bun:test';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter } from 'react-router-dom';
import { ObjectLink } from '@/features/objects/ObjectLink';
import { objectPath, type ObjectRef } from '@/features/objects/address';

const render = (object: ObjectRef, children?: string) =>
  renderToStaticMarkup(
    createElement(MemoryRouter, null, createElement(ObjectLink, { object }, children))
  );

const href = (markup: string) =>
  (markup.match(/href="([^"]*)"/)?.[1] ?? '').replaceAll('&amp;', '&');
const text = (markup: string) => markup.replace(/<[^>]+>/g, '');

describe('object link', () => {
  const objects: ObjectRef[] = [
    { kind: 'swarm', id: 's-1' },
    { kind: 'run', swarm: 's-1' },
    { kind: 'actor', project: 'p', swarm: 's', name: 'a' },
    { kind: 'template', project: 'p', swarm: 's', name: 't' },
    { kind: 'worker', name: 'w' },
    { kind: 'pool', name: 'n' },
    { kind: 'proxmoxPool', name: 'x' },
    { kind: 'workflow', project: 'p', name: 'release' },
  ];

  test('every type leads to its address', () => {
    for (const object of objects) {
      expect([object.kind, href(render(object))]).toEqual([object.kind, objectPath(object)]);
    }
  });

  test('a named object is labelled by its name unless the caller names it', () => {
    expect(text(render({ kind: 'actor', project: 'p', swarm: 's', name: 'builder' }))).toBe(
      'builder'
    );
    expect(text(render({ kind: 'pool', name: 'gvisor' }))).toBe('gvisor');
    expect(text(render({ kind: 'workflow', project: 'p', name: 'release' }))).toBe('release');
    expect(text(render({ kind: 'swarm', id: 's-1' }, 'docs crew'))).toBe('docs crew');
  });
});
