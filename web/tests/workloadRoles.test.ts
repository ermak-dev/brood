import { expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';

const source = (name: string) => readFileSync(new URL(`../src/features/substrate/${name}Page.tsx`, import.meta.url), 'utf8');

// FEATURE: Роль роя управляет доступностью workload-редакторов (#53).
// SPEC: Viewer читает таблицы и read-only managed details, но не редактирует
// native workload и не вызывает lifecycle; editor сохраняет workload-действия.
test('actor and template pages pass explicit swarm capabilities to every workload section', () => {
  for (const name of ['Actors', 'Templates']) {
    const page = source(name);
    expect(page).toContain('swarmCapabilities(swarm, admin).manage');
    expect(page).toContain('canManage: boolean');
    expect(page).toMatch(/const create = canManage\s*\?/);
    if (name === 'Templates') {
      // #157: template actions live in its panel; the builder takes the swarm role.
      expect(page).toMatch(/templateActionGroups\(viewing, \{[\s\S]*?canManage,/);
      expect(page).toMatch(/\{canManage && \(\s*<TemplateEditor/);
    } else {
      // #157: actions live in the actor's panel; builders take the swarm role.
      expect(page).toMatch(/nativeActionGroups\(editorActor, \{\s*t,\s*canManage,/);
      expect(page).toMatch(/\{dialog && canManage && \(\s*<ManagedActionDialog/);
      expect(page).toContain('canViewActorDetails(canManage, editor.actor)');
      expect(page).toContain('if (!canManage) return false;');
    }
  }
  expect(source('Actors')).toContain('!goldenOf(a) && canViewActorDetails(canManage, a)');
  expect(source('Templates')).toContain('onRowClick={(tpl) => setViewingKey(keyOf(tpl))}');
});
