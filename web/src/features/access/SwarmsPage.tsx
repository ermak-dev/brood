import { useCallback, useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import type { TFunction } from 'i18next';
import { useNavigate } from 'react-router-dom';
import { Button } from '@/components/ui/Button';
import { EmptyState } from '@/components/ui/EmptyState';
import { accessApi, type AccessProject, type AccessSwarm } from '@/services/brood/access';
import { EntityTable, type Column } from '@/features/substrate/components/EntityTable';
import { PageHeader } from '@/features/substrate/components/PageHeader';
import { DASH } from '@/features/substrate/format';
import { swarmPath } from '@/features/swarms/routes';
import { useSwarmActivityStore } from '@/stores';
import { AccessEditor, LoadState, StatusBadge, useAccessActions, useAccessResource } from './shared';
import { ProjectScope } from '@/features/substrate/components/ProjectScope';
import { ObjectLink } from '@/features/objects/ObjectLink';
import { swarmTone } from './state';
import { executionActivityTone, swarmActivityOf, swarmOrder } from './swarmActivity';
import { factColumns, swarmFactsOf, type SwarmFactColumns } from './swarmFacts';
import styles from '@/features/substrate/substrate.module.scss';

/** Рои глобально выбранного проекта; каталог уже отфильтрован сервером. */
export function SwarmsPage() {
  const { t } = useTranslation();
  return (
    <ProjectScope title={t('access.swarms')}>
      {(project) => <Swarms project={project} />}
    </ProjectScope>
  );
}

/** Таблица роёв (#255): инфраструктура и исполнение -- отдельные колонки,
 * ждущие ответа сверху. Сводку исполнения несёт строка списка (#234); без
 * неё -- прочерк. Определение снимка, участники и managed-акторы (#268) --
 * колонками, когда их несёт хоть одна строка: у старого сервера таблица
 * прежняя. Строка ведёт на страницу роя (#215). */
export function SwarmsTable({ swarms }: { swarms: AccessSwarm[] }) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const rows = [...swarms].sort(swarmOrder);
  const shown = factColumns(rows);
  // Имя вместо ID: полный ID с копированием -- на странице роя.
  const columns: Column<AccessSwarm>[] = [
    { key: 'name', label: t('substrate.name'), primary: true, render: (swarm) => swarm.name },
    {
      key: 'state',
      label: t('access.state'),
      render: (swarm) => (
        <StatusBadge tone={swarmTone(swarm.state)}>
          {t(`access.states.${swarm.state}`, { defaultValue: swarm.state })}
        </StatusBadge>
      ),
    },
    {
      key: 'execution',
      label: t('access.execution'),
      render: (swarm) => {
        const activity = swarmActivityOf(swarm);
        if (!activity) return DASH;
        return (
          <StatusBadge tone={executionActivityTone(activity.execution)}>
            {t(`access.execution_states.${activity.execution}`, {
              defaultValue: activity.execution,
            })}
          </StatusBadge>
        );
      },
    },
    {
      key: 'requests',
      label: t('access.open_requests'),
      render: (swarm) => {
        const activity = swarmActivityOf(swarm);
        if (!activity) return DASH;
        const open =
          activity.openRequests > 0 ? (
            <StatusBadge tone="attention">{activity.openRequests}</StatusBadge>
          ) : (
            String(activity.openRequests)
          );
        const undelivered = activity.undeliveredAnswers ?? 0;
        if (undelivered <= 0) return open;
        // Недоставленные ответы актору (#266) -- рядом, словами шапки инбокса (#264).
        return (
          <span className={styles.stateCell}>
            {open}
            <span title={t('access.undelivered_answers_hint')}>
              <StatusBadge tone="attention">
                {t('swarm_requests.undelivered_count', { count: undelivered })}
              </StatusBadge>
            </span>
          </span>
        );
      },
    },
    ...factColumnsOf(shown, t),
    {
      key: 'role',
      label: t('access.role'),
      hideOnMobile: true,
      render: (swarm) => t(`access.swarm_roles.${swarm.role}`),
    },
    {
      key: 'owner',
      label: t('access.owner'),
      render: (swarm) => swarm.owner_username,
    },
  ];
  return (
    <EntityTable
      columns={columns}
      rows={rows}
      rowKey={(swarm) => swarm.id}
      onRowClick={(swarm) => navigate(swarmPath(swarm.id))}
    />
  );
}

/** Колонки сведений #268, которые несёт хоть одна строка; у строки без
 * сведения -- прочерк. Число managed-акторов -- не все акторы роя: подпись
 * так и называет, подсказка говорит, что native-акторов в нём нет. */
function factColumnsOf(shown: SwarmFactColumns, t: TFunction): Column<AccessSwarm>[] {
  const columns: Column<AccessSwarm>[] = [];
  if (shown.definition) {
    columns.push({
      key: 'workflow',
      label: t('access.workflow'),
      render: (swarm) => {
        const { definition } = swarmFactsOf(swarm);
        if (!definition) return DASH;
        // Имя ведёт к определению в каталоге проекта (#270), а не туда, куда
        // строка: щелчок по ссылке страницу роя не открывает. Версия --
        // снимка: новая публикация каталога рой не меняет. Один элемент на
        // ячейку: на телефоне ячейка -- сетка «подпись -- значение».
        return (
          <span>
            <span onClick={(event) => event.stopPropagation()}>
              <ObjectLink
                object={{ kind: 'workflow', project: swarm.project_id, name: definition.name }}
              />
            </span>{' '}
            <span title={t('swarm_page.snapshot.hint')}>
              {t('swarm_page.snapshot.version_short', { version: definition.version })}
            </span>
          </span>
        );
      },
    });
  }
  if (shown.members) {
    columns.push({
      key: 'members',
      label: t('access.swarm_members'),
      render: (swarm) => String(swarmFactsOf(swarm).members ?? DASH),
    });
  }
  if (shown.managedActors) {
    columns.push({
      key: 'managed-actors',
      label: t('access.managed_actors'),
      render: (swarm) => {
        const count = swarmFactsOf(swarm).managedActors;
        if (count === undefined) return DASH;
        return <span title={t('access.managed_actors_hint')}>{count}</span>;
      },
    });
  }
  return columns;
}

/** Строка ведёт на страницу роя (#215): там его работа, а доступ, передача
 * и удаление -- в конце страницы. Здесь остаётся создание роя. Прочитанный
 * список страница отдаёт общему хранилищу: меню считает по нему ждущих. */
function Swarms({ project }: { project: AccessProject }) {
  const { t } = useTranslation();
  const projectId = project.id;
  const load = useCallback(() => accessApi.swarms(projectId), [projectId]);
  const resource = useAccessResource(load);
  const { run, busy } = useAccessActions(resource.refresh);
  const [creating, setCreating] = useState(false);
  const adopt = useSwarmActivityStore((state) => state.adopt);
  const swarms = resource.data;
  useEffect(() => {
    if (swarms) adopt(projectId, swarms);
  }, [adopt, projectId, swarms]);
  // Подпись о повторе -- только когда в списке есть незаконченная операция.
  const retryable =
    swarms?.some((swarm) => ['provisioning', 'error', 'deleting'].includes(swarm.state)) ?? false;
  const create = (
    <Button disabled={busy} onClick={() => setCreating(true)}>
      {t('access.create_swarm')}
    </Button>
  );
  return (
    <div className={styles.page}>
      <PageHeader title={t('access.swarms')} meta={[project.name]} actions={create} />
      <LoadState {...resource} retry={resource.refresh}>
        {swarms && swarms.length > 0 ? (
          <>
            <SwarmsTable swarms={swarms} />
            {retryable && <p className={styles.sectionHint}>{t('access.retry_hint')}</p>}
          </>
        ) : (
          <div className={styles.empty}>
            <EmptyState
              title={t('access.no_swarms')}
              description={t('access.swarm_access_hint')}
              action={create}
            />
          </div>
        )}
      </LoadState>
      {creating && (
        <AccessEditor
          key={projectId}
          title={t('access.create_swarm')}
          close={() => setCreating(false)}
          fields={[{ name: 'name', label: t('substrate.name'), required: true }]}
          submit={(values) => run(() => accessApi.createSwarm(projectId, values.name.trim()))}
        />
      )}
    </div>
  );
}
