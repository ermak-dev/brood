import { expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';

const source = (name: string) => readFileSync(new URL(`../src/features/substrate/${name}Page.tsx`, import.meta.url), 'utf8');

// FEATURE: Роль роя управляет доступностью workload-редакторов (#53).
// SPEC: Viewer читает таблицы, но не открывает редактор или терминал;
// смена роли убирает уже открытый редактор, а editor сохраняет workload-действия.
test('actor and template pages pass explicit swarm capabilities to every workload section', () => {
  for (const name of ['Actors', 'Templates']) {
    const page = source(name);
    expect(page).toContain('swarmCapabilities(swarm, admin).manage');
    expect(page).toContain('canManage: boolean');
    expect(page).toMatch(/const create = canManage\s*\?/);
    expect(page).toMatch(/actions=\{\s*canManage\s*\?/);
    expect(page).toMatch(new RegExp(`\\{canManage && \\(\\s*<${name === 'Actors' ? 'Actor' : 'Template'}Editor`));
  }
  expect(source('Actors')).toContain('onRowClick={canManage ?');
  expect(source('Templates')).toMatch(/viewing &&\s*canManage &&/);
  expect(source('Templates')).toContain('onRowClick={setViewing}');
});
