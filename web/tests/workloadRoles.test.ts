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
      // #194: the new-actor editor and an existing actor's panel are separate gates.
      expect(page).toContain('canViewActorDetails(canManage, null)');
      expect(page).toContain('canViewActorDetails(canManage, editorActor)');
      expect(page).toContain('if (!canManage) return false;');
    }
  }
  expect(source('Actors')).toContain('!goldenOf(a) && canViewActorDetails(canManage, a)');
  // #194: a row opens the template's panel through its address.
  expect(source('Templates')).toContain('onRowClick={(tpl) => openTemplate(tpl.metadata.name)}');
});
