// FEATURE: Действия страниц доступа живут в боковой панели объекта (#157).
// SPEC: Строка открывает панель; панель предлагает ровно те действия, что
// давали кнопки строки, с теми же условиями прав. Главное действие первое,
// необратимые -- отдельным блоком, участник без прав видит панель только
// для чтения.
import { describe, expect, test } from 'bun:test';
import type { TFunction } from 'i18next';
import type { AccessSwarm } from '@/services/brood/access';
import {
  memberPlan,
  panelGroups,
  projectPlan,
  swarmPlan,
  userPlan,
} from '@/features/access/panelActions';

const t = ((key: string) => key) as unknown as TFunction;

const swarm = (patch: Partial<AccessSwarm>): AccessSwarm => ({
  id: 'swarm-1',
  project_id: 'project-1',
  owner_id: 'owner-1',
  owner_username: 'alice',
  role: 'owner',
  retry: '',
  name: 'alpha',
  atespace_name: 'alpha',
  state: 'active',
  created_at: '',
  updated_at: '',
  ...patch,
});

describe('projects', () => {
  test('everyone can select a project or open its details; selecting comes first', () => {
    expect(projectPlan(false)).toEqual({ main: ['select', 'details'], danger: [] });
  });

  test('only an administrator deletes a project, and deletion is irreversible', () => {
    expect(projectPlan(true)).toEqual({ main: ['select', 'details'], danger: ['delete'] });
  });
});

describe('swarms', () => {
  test('a viewer or editor of an active swarm only opens its access list', () => {
    for (const role of ['viewer', 'editor'] as const) {
      expect(swarmPlan(swarm({ role }), false)).toEqual({ main: ['access'], danger: [] });
    }
  });

  test('an owner of an active swarm also transfers and deletes it, both irreversible', () => {
    expect(swarmPlan(swarm({ role: 'owner' }), false)).toEqual({
      main: ['access'],
      danger: ['transfer', 'delete'],
    });
  });

  test('an administrator owns every swarm, whatever its grant', () => {
    expect(swarmPlan(swarm({ role: 'viewer' }), true)).toEqual({
      main: ['access'],
      danger: ['transfer', 'delete'],
    });
  });

  test('a provisioning swarm offers its owner only to resume creation', () => {
    expect(swarmPlan(swarm({ state: 'provisioning' }), false)).toEqual({
      main: ['retry_create'],
      danger: [],
    });
  });

  test('a failed swarm resumes the operation it saved, and can always be deleted', () => {
    expect(swarmPlan(swarm({ state: 'error', retry: 'create' }), false)).toEqual({
      main: ['retry_create'],
      danger: ['delete'],
    });
    expect(swarmPlan(swarm({ state: 'error', retry: 'delete' }), false)).toEqual({
      main: [],
      danger: ['delete'],
    });
    expect(swarmPlan(swarm({ state: 'deleting' }), false)).toEqual({
      main: [],
      danger: ['delete'],
    });
  });

  test('access is listed only for an active swarm; nothing is offered to a non-owner otherwise', () => {
    for (const state of ['provisioning', 'error', 'deleting', 'deleted']) {
      expect(swarmPlan(swarm({ state, role: 'editor', retry: 'create' }), false)).toEqual({
        main: [],
        danger: [],
      });
    }
  });
});

test('users: edit first, password reset next, deletion irreversible', () => {
  expect(userPlan()).toEqual({ main: ['edit', 'password'], danger: ['delete'] });
});

describe('project members', () => {
  test('an administrator removes a member as an irreversible action', () => {
    expect(memberPlan(true)).toEqual({ main: [], danger: ['remove'] });
  });

  test('a project member reads the panel without any action', () => {
    expect(memberPlan(false)).toEqual({ main: [], danger: [] });
    expect(panelGroups(memberPlan(false), { remove: { label: 'Remove' } }, t)).toEqual([]);
  });
});

describe('a plan becomes panel groups', () => {
  const specs = {
    select: { label: 'Select', description: 'select it', onSelect: () => {} },
    details: { label: 'Details' },
    delete: { label: 'Delete', pending: true },
  };

  test('the first ordinary action is primary and every irreversible one is danger', () => {
    const groups = panelGroups(projectPlan(true), specs, t);
    expect(groups.map((group) => [group.id, group.label, Boolean(group.danger)])).toEqual([
      ['main', 'substrate.actions', false],
      ['danger', 'substrate.panel_danger', true],
    ]);
    expect(groups[0].actions.map((action) => [action.id, action.tone])).toEqual([
      ['select', 'primary'],
      ['details', undefined],
    ]);
    expect(groups[1].actions.map((action) => [action.id, action.tone])).toEqual([
      ['delete', 'danger'],
    ]);
  });

  test('labels, descriptions, handlers and pending state come from the page unchanged', () => {
    const [main, danger] = panelGroups(projectPlan(true), specs, t);
    expect(main.actions[0]).toMatchObject({ label: 'Select', description: 'select it' });
    expect(main.actions[0].onSelect).toBe(specs.select.onSelect);
    expect(danger.actions[0].pending).toBe(true);
  });

  test('an empty group is not rendered at all', () => {
    expect(panelGroups(projectPlan(false), specs, t).map((group) => group.id)).toEqual(['main']);
    expect(
      panelGroups(
        swarmPlan(swarm({ state: 'deleting' }), false),
        {
          access: { label: 'Access' },
          retry_create: { label: 'Retry' },
          transfer: { label: 'Transfer' },
          delete: { label: 'Delete' },
        },
        t
      ).map((group) => group.id)
    ).toEqual(['danger']);
  });
});
