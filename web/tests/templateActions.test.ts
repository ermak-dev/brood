// FEATURE: Действия шаблона живут в боковой панели (#157), а не в строке таблицы.
// SPEC: шаблон неизменяем, поэтому «Создать копию» -- главное действие и
// открывает редактор сразу; удаление необратимо и идёт через подтверждение.
// Профиль Proxmox (readOnly) копировать и удалять нельзя, причина названа.
// Без права управления роем действий нет.
import { describe, expect, test } from 'bun:test';
import type { TFunction } from 'i18next';
import type { ActorTemplate } from '@/services/brood/substrate';
import type { PanelActionGroup } from '@/features/substrate/components/EntityPanel';
import type { Confirmation } from '@/features/substrate/poolActions';
import { isManagedTemplate, templateActionGroups } from '@/features/substrate/templateActions';

const t = ((key: string, options?: { name?: string }) =>
  options?.name ? `${key}(${options.name})` : key) as unknown as TFunction;

const template: ActorTemplate = {
  metadata: { name: 'claude' },
  containers: [{ name: 'main', image: 'registry/claude@sha256:abc' }],
};
const managed: ActorTemplate = {
  metadata: { name: 'proxmox-limited' }, provider: 'proxmox', readOnly: true, profile: 'limited',
};

const shape = (groups: PanelActionGroup[]) =>
  groups.map((group) => ({ id: group.id, danger: Boolean(group.danger), actions: group.actions.map((a) => a.id) }));

function build(target: ActorTemplate, canManage = true) {
  const asked: Confirmation[] = [];
  const calls: string[] = [];
  const groups = templateActionGroups(target, {
    t,
    canManage,
    confirm: (confirmation) => asked.push(confirmation),
    duplicate: () => calls.push('duplicate'),
    remove: async () => calls.push('remove'),
  });
  return { groups, asked, calls };
}

describe('template panel actions', () => {
  test('duplicate is the primary action, delete sits alone in the irreversible group', () => {
    const { groups } = build(template);
    expect(shape(groups)).toEqual([
      { id: 'main', danger: false, actions: ['duplicate'] },
      { id: 'danger', danger: true, actions: ['delete'] },
    ]);
    expect(groups[0].actions[0]).toMatchObject({
      label: 'templates.duplicate', description: 'templates.duplicate_hint', tone: 'primary',
    });
    expect(groups[0].actions[0].blocked).toBeUndefined();
    expect(groups[1].actions[0]).toMatchObject({
      label: 'substrate.delete', description: 'templates.delete_hint', tone: 'danger',
    });
  });

  test('duplicate opens the editor at once; delete asks and removes only after confirmation', async () => {
    const { groups, asked, calls } = build(template);
    groups[0].actions[0].onSelect!();
    expect(calls).toEqual(['duplicate']);
    expect(asked).toEqual([]);
    groups[1].actions[0].onSelect!();
    expect(calls).toEqual(['duplicate']);
    expect(asked).toHaveLength(1);
    expect(asked[0]).toMatchObject({
      title: 'templates.delete_title',
      message: 'templates.delete_message(claude)',
      confirmText: 'substrate.delete',
      success: 'templates.deleted(claude)',
    });
    await asked[0].run();
    expect(calls).toEqual(['duplicate', 'remove']);
  });

  test('a managed Proxmox profile cannot be copied or deleted, and says why', () => {
    for (const target of [managed, { ...managed, readOnly: undefined }, { ...template, readOnly: true }]) {
      expect(isManagedTemplate(target)).toBe(true);
      const { groups, asked, calls } = build(target);
      expect(shape(groups)).toEqual([{ id: 'main', danger: false, actions: ['duplicate'] }]);
      expect(groups[0].actions[0].blocked).toBe('templates.managed_readonly');
      expect(groups[0].actions[0].onSelect).toBeUndefined();
      expect(asked).toEqual([]);
      expect(calls).toEqual([]);
    }
    expect(isManagedTemplate(template)).toBe(false);
  });

  test('without the right to manage the project there are no actions at all', () => {
    for (const target of [template, managed]) expect(build(target, false).groups).toEqual([]);
  });
});
