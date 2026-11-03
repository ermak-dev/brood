import type { TFunction } from 'i18next';
import type { ActorTemplate } from '@/services/brood/substrate';
import type { PanelActionGroup } from './components/EntityPanel';
import type { Confirmation } from './poolActions';

/** Профиль Proxmox публикует сервер из управляемого пула (readOnly): он
 * меняется вместе с пулом, копия или удаление здесь его сломали бы. */
export const isManagedTemplate = (template: ActorTemplate) =>
  Boolean(template.readOnly) || template.provider === 'proxmox';

/** Действия шаблона в панели. ateapi держит шаблоны неизменяемыми, поэтому
 * правка -- это копия в редакторе, и она главное действие. Без права
 * управления роем действий нет совсем; у управляемого профиля копия
 * заблокирована с причиной, а удаления нет. */
export function templateActionGroups(
  template: ActorTemplate,
  {
    t,
    canManage,
    confirm,
    duplicate,
    remove,
  }: {
    t: TFunction;
    canManage: boolean;
    confirm: (confirmation: Confirmation) => void;
    duplicate: () => void;
    remove: () => Promise<unknown>;
  }
): PanelActionGroup[] {
  if (!canManage) return [];
  const name = template.metadata.name;
  const managed = isManagedTemplate(template);
  const main: PanelActionGroup = {
    id: 'main',
    label: t('substrate.actions'),
    actions: [
      {
        id: 'duplicate',
        label: t('templates.duplicate'),
        description: t('templates.duplicate_hint'),
        tone: 'primary',
        blocked: managed ? t('templates.managed_readonly') : undefined,
        onSelect: managed ? undefined : duplicate,
      },
    ],
  };
  if (managed) return [main];
  return [
    main,
    {
      id: 'danger',
      label: t('substrate.panel_danger'),
      danger: true,
      actions: [
        {
          id: 'delete',
          label: t('substrate.delete'),
          description: t('templates.delete_hint'),
          tone: 'danger',
          onSelect: () =>
            confirm({
              title: t('templates.delete_title'),
              message: t('templates.delete_message', { name }),
              confirmText: t('substrate.delete'),
              success: t('templates.deleted', { name }),
              run: remove,
            }),
        },
      ],
    },
  ];
}
