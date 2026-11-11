import { useCallback, useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { Select } from '@/components/ui/Select';
import type { AccessSwarm } from '@/services/brood/access';
import type { PipelineRun } from '@/services/brood/pipelines';
import { rolesApi, type GraphRole, type RoleActor, type RoleGraph } from '@/services/brood/roles';
import { LoadState, StatusBadge, useAdministrator } from '@/features/access/shared';
import { ObjectLink } from '@/features/objects/ObjectLink';
import type { SwarmScope } from '@/features/objects/relations';
import { useResource } from '@/features/substrate/hooks';
import { DASH } from '@/features/substrate/format';
import { Fold } from './Fold';
import { actorChoice, roleTone, unreadyRoles } from './roleState';
import { rolesUnfolded } from './swarmState';
import { useSwarmRoles, type SwarmRolesModel } from './useSwarmRoles';
import styles from '@/features/substrate/substrate.module.scss';
import page from './SwarmPage.module.scss';
import local from './SwarmRoles.module.scss';

type Translate = ReturnType<typeof useTranslation>['t'];

const stateName = (t: Translate, state: string) =>
  t(`swarm_roles.states.${state}`, { defaultValue: state });

/** Блок ролей страницы роя (#241). definition — определение, выбранное в
 * диалоге первого запуска: у роя без снимка роли — его. */
export function SwarmRoles({
  swarm,
  run,
  definition,
}: {
  swarm: AccessSwarm;
  /** Исполнение роя: null — снимка нет, undefined — ещё не прочитано. */
  run: PipelineRun | null | undefined;
  definition: string;
}) {
  const administrator = useAdministrator();
  return (
    <RolesSection
      model={useSwarmRoles(swarm, administrator, run, definition)}
      scope={{ project: swarm.project_id, swarm: swarm.id }}
    />
  );
}

/** Итог блока ролей в его summary: сколько ролей графа не готово, иначе
 * сколько готово. Без представления итога нет. */
function rolesSummary(t: Translate, view: SwarmRolesModel['view']): string | undefined {
  if (!view) return undefined;
  const unready = unreadyRoles(view).length;
  if (unready > 0) return t('swarm_roles.fold_unready', { count: unready });
  const ready = view.roles.filter((role) => role.inGraph).length;
  return ready > 0 ? t('swarm_roles.fold_ready', { count: ready }) : undefined;
}

/** Роли графа и привязки роя: состояние словами, актор по имени -- ссылкой
 * на его панель (#195). Блок свёрнут (#253), пока рой работает с готовыми
 * ролями; раскрыт, когда роль ждёт человека или рой стоит. Пока исполнение
 * идёт или ждёт ответа, а также для зрителя управления нет — вместо него
 * текст, почему. scope -- проект и рой: ссылки ведут к акторам этого роя. */
export function RolesSection({ model, scope }: { model: SwarmRolesModel; scope: SwarmScope }) {
  const { t } = useTranslation();
  const { view } = model;
  return (
    <Fold
      title={t('swarm_roles.title')}
      summary={rolesSummary(t, view)}
      hint={t('swarm_roles.hint')}
      open={rolesUnfolded(view)}
      data-swarm-roles=""
    >
      <LoadState
        loading={view === null && !model.error}
        error={view === null ? model.error : ''}
        retry={model.refresh}
      >
        {view && (
          <div className={local.block}>
            {model.error && (
              <p className={`${styles.notice} ${styles.operationFailure}`} role="alert">
                {model.error}
              </p>
            )}
            <GraphLine graph={view.graph} />
            <ControlNote model={model} scope={scope} />
            {view.roles.length === 0 ? (
              <p className={page.hint}>
                {t(view.graph ? 'swarm_roles.empty' : 'swarm_roles.empty_no_graph')}
              </p>
            ) : (
              <ul className={local.list}>
                {view.roles.map((role) => (
                  <RoleRow
                    key={role.role}
                    role={role}
                    graph={view.graph}
                    model={model}
                    scope={scope}
                  />
                ))}
              </ul>
            )}
            {droppedRoles(model).map((role) => (
              <RoleOutcome key={role} role={role} model={model} />
            ))}
          </div>
        )}
      </LoadState>
    </Fold>
  );
}

/** Откуда роли: снимок роя, определение из диалога запуска или графа нет. */
function GraphLine({ graph }: { graph: RoleGraph | null }) {
  const { t } = useTranslation();
  if (!graph) return <p className={page.hint}>{t('swarm_roles.graph_none')}</p>;
  const values = { name: graph.name, version: graph.version };
  return (
    <p className={page.hint}>
      {t(graph.pinned ? 'swarm_roles.graph_pinned' : 'swarm_roles.graph_definition', values)}
    </p>
  );
}

/** Почему управления нет, или чего не хватает для него: одной строкой на
 * блок, а не неактивными кнопками в каждой роли. */
function ControlNote({ model, scope }: { model: SwarmRolesModel; scope: SwarmScope }) {
  const { t } = useTranslation();
  if (model.control === 'busy') {
    return <p className={`${styles.notice} ${local.warning}`}>{t('swarm_roles.busy')}</p>;
  }
  if (model.control === 'read') return <p className={page.hint}>{t('swarm_roles.read_only')}</p>;
  if (model.actorsError) {
    return (
      <div className={`${styles.notice} ${styles.operationFailure}`} role="alert">
        <p>{t('swarm_roles.actors_error', { reason: model.actorsError })}</p>
        <Button variant="secondary" size="sm" onClick={() => void model.refreshActors()}>
          {t('access.refresh')}
        </Button>
      </div>
    );
  }
  if (model.actors?.length === 0) {
    return (
      <p className={page.hint}>
        {t('swarm_roles.actors_none')}{' '}
        <ObjectLink object={{ kind: 'actor', ...scope }}>{t('swarm_roles.actors_link')}</ObjectLink>
      </p>
    );
  }
  const roles = model.view?.roles.length ?? 0;
  return model.actors && roles > 0 ? (
    <p className={page.hint}>{t('swarm_roles.actors_hint')}</p>
  ) : null;
}

/** Снятые роли, которых больше нет в представлении: граф их не называет, и
 * строка ушла вместе с привязкой. Итог остаётся в блоке, а не пропадает. */
function droppedRoles(model: SwarmRolesModel): string[] {
  const shown = new Set(model.view?.roles.map((role) => role.role));
  return Object.entries(model.notices)
    .filter(([role, notice]) => notice.unbound && !shown.has(role))
    .map(([role]) => role);
}

/** Итог последней записи роли: кому привязана, снята или отказ сервера.
 * Нажатая кнопка на время записи занята и теряет фокус, снятая привязка
 * уносит её вовсе: фокус переходит к итогу, один раз. */
function RoleOutcome({ role, model }: { role: string; model: SwarmRolesModel }) {
  const { t } = useTranslation();
  const notice = model.notices[role];
  const ref = useRef<HTMLParagraphElement>(null);
  const { focused } = model;
  const shown = Boolean(notice?.bound || notice?.unbound || notice?.refusal);
  const takeFocus = shown && model.focus === role;
  useEffect(() => {
    if (!takeFocus || !ref.current) return;
    ref.current.focus();
    focused();
  }, [takeFocus, focused]);
  if (notice?.refusal) {
    return (
      <p ref={ref} tabIndex={-1} role="alert" className={local.refusal}>
        {t('swarm_roles.refused', { reason: notice.refusal })}
      </p>
    );
  }
  if (!notice?.bound && !notice?.unbound) return null;
  return (
    <p ref={ref} tabIndex={-1} role="status" className={local.done}>
      {notice.bound
        ? t('swarm_roles.bound', { role, actor: notice.bound })
        : t('swarm_roles.unbound_done', { role })}
    </p>
  );
}

/** Актор роли по имени, ссылкой на его панель: роль держит managed-актор
 * этого роя, его панель открыта и зрителю. Имя актора чужого роя сервер не
 * раскрывает. */
function RoleActorName({ role, scope }: { role: GraphRole; scope: SwarmScope }) {
  const { t } = useTranslation();
  if (role.actorName) {
    return <ObjectLink object={{ kind: 'actor', ...scope, name: role.actorName }} />;
  }
  return <>{actorLabel(t, role)}</>;
}

function actorLabel(t: Translate, role: GraphRole): string {
  if (!role.actorUid) return t('swarm_roles.actor_none');
  if (role.state === 'foreign') return t('swarm_roles.actor_hidden');
  if (role.state === 'missing') return t('swarm_roles.actor_gone');
  return DASH;
}

function RoleRow({
  role,
  graph,
  model,
  scope,
}: {
  role: GraphRole;
  graph: RoleGraph | null;
  model: SwarmRolesModel;
  scope: SwarmScope;
}) {
  const { t } = useTranslation();
  return (
    <li className={local.row} data-role={role.role} data-state={role.state}>
      <div className={local.head}>
        <code className={local.name}>{role.role}</code>
        {graph && !role.inGraph && (
          <span className={local.tag}>{t('swarm_roles.not_in_graph')}</span>
        )}
        <StatusBadge tone={roleTone(role.state)}>{stateName(t, role.state)}</StatusBadge>
      </div>
      <dl className={local.facts}>
        <div>
          <dt>{t('swarm_roles.actor')}</dt>
          <dd>
            <RoleActorName role={role} scope={scope} />
          </dd>
        </div>
      </dl>
      <p className={page.hint}>
        {t(`swarm_roles.state_hints.${role.state}`, { defaultValue: '' })}
      </p>
      {model.control === 'edit' && <RoleForm role={role} model={model} />}
      <RoleOutcome role={role.role} model={model} />
    </li>
  );
}

/** Выбор актора и действия роли. Предложены только акторы, которых сервер
 * примет; привязка уходит с инкарнацией, показанной в списке. */
function RoleForm({ role, model }: { role: GraphRole; model: SwarmRolesModel }) {
  const { t } = useTranslation();
  const [chosen, setChosen] = useState<string>();
  const [action, setAction] = useState<'bind' | 'unbind'>();
  const offered = model.actors ?? [];
  const choice = actorChoice(offered, role, chosen);
  const bound = Boolean(role.actorUid);
  const writing = model.pending !== null;
  const mine = model.pending === role.role;
  const phase = (actor: RoleActor) =>
    t(`swarm_roles.phases.${actor.phase}`, { defaultValue: actor.phase });
  return (
    <div className={local.form}>
      {offered.length > 0 && (
        <>
          <Select
            value={choice?.uid ?? ''}
            options={offered.map((actor) => ({
              value: actor.uid,
              label: t('swarm_roles.actor_option', { name: actor.name, phase: phase(actor) }),
            }))}
            onChange={setChosen}
            placeholder={t('swarm_roles.choose_placeholder')}
            ariaLabel={t('swarm_roles.choose', { role: role.role })}
            disabled={writing}
            fullWidth={false}
            size="sm"
            className={local.select}
          />
          <Button
            size="sm"
            aria-label={t(bound ? 'swarm_roles.rebind_role' : 'swarm_roles.bind_role', {
              role: role.role,
            })}
            disabled={!choice || writing}
            loading={mine && action === 'bind'}
            onClick={() => {
              if (!choice) return;
              setAction('bind');
              void model.bind(role.role, choice);
            }}
          >
            {t(bound ? 'swarm_roles.rebind' : 'swarm_roles.bind')}
          </Button>
        </>
      )}
      {bound && (
        <Button
          variant="ghost"
          size="sm"
          aria-label={t('swarm_roles.unbind_role', { role: role.role })}
          disabled={writing}
          loading={mine && action === 'unbind'}
          onClick={() => {
            setAction('unbind');
            void model.unbind(role.role);
          }}
        >
          {t('swarm_roles.unbind')}
        </Button>
      )}
    </div>
  );
}

/** Подсказка диалога запуска (#241): роли графа, которые запуск отвергнет,
 * видны до нажатия. definition — определение первого запуска; '' — снимок
 * роя при повторном. Это подсказка, а не запрет: судит сервер. */
export function StartRolesNote({
  id,
  swarmId,
  definition,
}: {
  id: string;
  swarmId: string;
  definition: string;
}) {
  const { t } = useTranslation();
  const load = useCallback(() => rolesApi.list(swarmId, definition), [swarmId, definition]);
  const roles = useResource(load, { headerRefresh: false });
  if (roles.error) {
    return (
      <p id={id} data-roles-note="" className={styles.fieldHint}>
        {t('swarm_roles.start.failed', { reason: roles.error })}
      </p>
    );
  }
  if (!roles.data) {
    return (
      <p id={id} data-roles-note="" className={styles.fieldHint}>
        {t('swarm_roles.start.checking')}
      </p>
    );
  }
  if (!roles.data.roles.some((role) => role.inGraph)) return null;
  const unready = unreadyRoles(roles.data);
  if (unready.length === 0) {
    return (
      <p id={id} data-roles-note="" className={styles.notice}>
        {t('swarm_roles.start.ready')}
      </p>
    );
  }
  return (
    <div id={id} data-roles-note="" className={`${styles.notice} ${local.warning}`}>
      <p className={local.noteText}>{t('swarm_roles.start.unready')}</p>
      <ul className={local.unready}>
        {unready.map((role) => (
          <li key={role.role}>
            <code className={local.name}>{role.role}</code>: {stateName(t, role.state)}
          </li>
        ))}
      </ul>
      <p className={local.noteText}>{t('swarm_roles.start.where')}</p>
    </div>
  );
}
