import { useCallback, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { accessApi, type AccessSwarm } from '@/services/brood/access';
import { PanelActions, type PanelActionGroup } from '@/features/substrate/components/EntityPanel';
import { Section } from '@/features/substrate/components/form';
import { panelGroups, swarmPlan, type ActionSpec, type SwarmAction } from './panelActions';
import { AccessEditor, useAccessActions, useAccessResource, useAdministrator } from './shared';
import { ownerCandidates } from './state';
import { swarmCapabilities } from './shardState';
import { SwarmAccess } from './SwarmAccess';

/** На странице роя главное — его работа: действия доступа не заливаются
 * как главное действие объекта, необратимые сохраняют свой тон. */
function secondary(groups: PanelActionGroup[]): PanelActionGroup[] {
  return groups.map((group) => ({
    ...group,
    actions: group.actions.map((action) =>
      action.tone === 'primary' ? { ...action, tone: undefined } : action
    ),
  }));
}

/** Доступ и жизненный цикл роя (#215): то, что раньше занимало панель роя,
 * теперь в конце его страницы. План действий тот же (swarmPlan), с теми же
 * подтверждениями; необратимые — отдельным блоком. Доступ и передача
 * открывают свои редакторы; повтор создания идёт на месте. После удаления
 * страница роя уходит к списку (deleted). Сервер проверяет каждое действие. */
export function SwarmManagement({
  swarm,
  changed,
  deleted,
}: {
  swarm: AccessSwarm;
  changed: () => Promise<void>;
  deleted: () => void;
}) {
  const { t } = useTranslation();
  const admin = useAdministrator();
  const { run, confirm, busy } = useAccessActions(changed);
  const [transferring, setTransferring] = useState(false);
  const [viewingAccess, setViewingAccess] = useState(false);
  const canTransfer = swarmCapabilities(swarm, admin).own;
  const projectId = swarm.project_id;
  // Участники нужны только открытой передаче владельцем: иначе не грузятся.
  const choosingOwner = transferring && canTransfer;
  const loadMembers = useCallback(
    () => (choosingOwner ? accessApi.members(projectId) : Promise.resolve([])),
    [choosingOwner, projectId]
  );
  const members = useAccessResource(loadMembers);
  const owners = ownerCandidates(members.data, swarm.owner_username);
  const remove = () =>
    confirm(swarm.name, async () => {
      await accessApi.deleteSwarm(swarm.id);
      deleted();
    });
  const retryingDelete = ['error', 'deleting'].includes(swarm.state);
  const actions: Record<SwarmAction, ActionSpec> = {
    access: {
      label: t('access.swarm_access'),
      description: t('access.panel.swarm_access_hint'),
      onSelect: () => setViewingAccess(true),
    },
    retry_create: {
      label: t('access.retry_create'),
      description: t('access.panel.retry_create_hint'),
      pending: busy,
      onSelect: () => void run(() => accessApi.createSwarm(swarm.project_id, swarm.name)),
    },
    transfer: {
      label: t('access.transfer'),
      description: t('access.panel.transfer_hint'),
      pending: busy,
      onSelect: () => setTransferring(true),
    },
    delete: {
      label: t(retryingDelete ? 'access.retry_delete' : 'substrate.delete'),
      description: t(
        retryingDelete ? 'access.panel.retry_delete_hint' : 'access.panel.delete_swarm_hint'
      ),
      pending: busy,
      onSelect: remove,
    },
  };
  return (
    <Section title={t('swarm_page.management')} hint={t('swarm_page.management_hint')}>
      <PanelActions groups={secondary(panelGroups(swarmPlan(swarm, admin), actions, t))} />
      {choosingOwner && (
        <AccessEditor
          key={swarm.id}
          title={t('access.transfer_for', { name: swarm.name })}
          description={t('access.transfer_hint')}
          close={() => setTransferring(false)}
          fields={[
            {
              name: 'username',
              label: t('access.username'),
              type: 'select',
              required: true,
              options: owners,
              empty:
                members.error || (members.loading ? t('common.loading') : t('access.no_members')),
            },
          ]}
          submit={(values) => run(() => accessApi.transferSwarm(swarm.id, values.username))}
        />
      )}
      {viewingAccess && (
        <SwarmAccess
          key={swarm.id}
          swarm={swarm}
          close={() => setViewingAccess(false)}
          changed={changed}
        />
      )}
    </Section>
  );
}
