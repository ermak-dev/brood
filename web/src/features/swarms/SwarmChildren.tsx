import { useCallback, useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { PlainText } from '@/components/ui/Markdown';
import { LoadState, StatusBadge, useAdministrator } from '@/features/access/shared';
import type { AccessSwarm } from '@/services/brood/access';
import {
  childrenApi,
  type ChildGroup,
  type ChildGroupSummary,
  type ChildTask,
} from '@/services/brood/children';
import { BroodApiError, errorMessage } from '@/services/brood/http';
import { useHeaderRefresh } from '@/hooks/useHeaderRefresh';
import { ConfirmDialog } from '@/features/substrate/components/ConfirmDialog';
import { useResource } from '@/features/substrate/hooks';
import { formatDateTimeValue } from '@/utils/format';
import { canAbortChildGroup } from './childState';
import { Fold } from './Fold';
import { useSwarmTick } from './useSwarmPolling';
import styles from '@/features/substrate/substrate.module.scss';
import local from './SwarmChildren.module.scss';

type Translate = ReturnType<typeof useTranslation>['t'];
const stateName = (t: Translate, kind: string, state: string) =>
  t(`swarm_children.${kind}.${state}`, { defaultValue: state });

function policyText(t: Translate, group: ChildGroupSummary) {
  return group.mode === 'quorum'
    ? t('swarm_children.policy_quorum', { quorum: group.quorum, total: group.total })
    : t('swarm_children.policy', { mode: stateName(t, 'modes', group.mode) });
}

export function SwarmChildren({ swarm }: { swarm: AccessSwarm }) {
  const administrator = useAdministrator();
  return <ChildGroupsPane swarm={swarm} owner={administrator || swarm.role === 'owner'} />;
}

/** Новый scope не наследует выбор группы, форму и поздний ответ прежнего роя. */
export function ChildGroupsPane(props: { swarm: AccessSwarm; owner: boolean }) {
  return <ChildGroupsScreen key={props.swarm.id} {...props} />;
}

function ChildGroupsScreen({ swarm, owner }: { swarm: AccessSwarm; owner: boolean }) {
  const [cursors, setCursors] = useState([0]);
  const [selected, setSelected] = useState<number | null>(null);
  const before = cursors[cursors.length - 1];
  return (
    <ChildHistoryPage
      key={before}
      swarmId={swarm.id}
      owner={owner}
      before={before}
      selected={selected}
      select={(sequence) => setSelected((value) => (value === sequence ? null : sequence))}
      hasNewer={cursors.length > 1}
      newer={() => {
        setSelected(null);
        setCursors((values) => values.slice(0, -1));
      }}
      older={(cursor) => {
        setSelected(null);
        setCursors((values) => [...values, cursor]);
      }}
    />
  );
}

function ChildHistoryPage({
  swarmId,
  owner,
  before,
  selected,
  select,
  hasNewer,
  newer,
  older,
}: {
  swarmId: string;
  owner: boolean;
  before: number;
  selected: number | null;
  select: (sequence: number) => void;
  hasNewer: boolean;
  newer: () => void;
  older: (cursor: number) => void;
}) {
  const { t } = useTranslation();
  const load = useCallback(() => childrenApi.list(swarmId, before), [swarmId, before]);
  const page = useResource(load, { trackRefresh: true });
  const { loading: pageLoading, refresh: refreshPage } = page;
  useSwarmTick(
    useCallback(() => {
      if (!pageLoading) void refreshPage();
    }, [pageLoading, refreshPage])
  );
  const groups = page.data?.items ?? [];
  const attention = groups.some((group) => group.interrupted || group.open > 0);
  return (
    <Fold
      title={t('swarm_children.title')}
      hint={t('swarm_children.hint')}
      open={attention || selected !== null || Boolean(page.error)}
      data-child-groups=""
    >
      <LoadState
        loading={!page.data && page.loading}
        error={!page.data ? (page.error ?? '') : ''}
        retry={page.refresh}
      >
        {page.data && (
          <div className={local.block}>
            {page.error && (
              <p className={`${styles.notice} ${styles.operationFailure}`} role="alert">
                {page.error}
              </p>
            )}
            {groups.length === 0 ? (
              <p className={local.hint}>{t('swarm_children.empty')}</p>
            ) : (
              <ul className={local.groups}>
                {groups.map((group) => (
                  <li key={group.parentSequence} className={local.groupRow}>
                    <div className={local.head}>
                      <Button
                        variant="ghost"
                        size="sm"
                        data-child-select={group.parentSequence}
                        aria-pressed={selected === group.parentSequence}
                        onClick={() => select(group.parentSequence)}
                      >
                        {t('swarm_children.group', {
                          sequence: group.parentSequence,
                          pass: group.pass,
                        })}
                      </Button>
                      <StatusBadge
                        tone={
                          group.interrupted ? 'attention' : group.open > 0 ? 'progress' : 'neutral'
                        }
                      >
                        {group.interrupted
                          ? stateName(t, 'readiness_states', 'unconfirmed')
                          : stateName(t, 'group_states', group.state)}
                      </StatusBadge>
                    </div>
                    <div className={local.facts}>
                      <span>{policyText(t, group)}</span>
                      <span>
                        {t('swarm_children.registered', {
                          registered: group.registered,
                          total: group.total,
                        })}
                      </span>
                      <span>{t('swarm_children.open', { count: group.open })}</span>
                    </div>
                  </li>
                ))}
              </ul>
            )}
            <div className={local.actions}>
              {hasNewer && (
                <Button variant="secondary" size="sm" disabled={page.loading} onClick={newer}>
                  {t('swarm_children.newer')}
                </Button>
              )}
              {page.data.nextBefore > 0 && (
                <Button
                  variant="secondary"
                  size="sm"
                  disabled={page.loading}
                  onClick={() => older(page.data!.nextBefore)}
                >
                  {t('swarm_children.older')}
                </Button>
              )}
              <Button
                variant="ghost"
                size="sm"
                loading={page.loading}
                onClick={() => void page.refresh()}
              >
                {t('swarm_children.refresh')}
              </Button>
            </div>
            {selected !== null && (
              <ChildGroupDetail
                key={selected}
                swarmId={swarmId}
                sequence={selected}
                owner={owner}
                changed={page.refresh}
              />
            )}
          </div>
        )}
      </LoadState>
    </Fold>
  );
}

function ChildGroupDetail({
  swarmId,
  sequence,
  owner,
  changed,
}: {
  swarmId: string;
  sequence: number;
  owner: boolean;
  changed: () => Promise<void>;
}) {
  const { t } = useTranslation();
  const load = useCallback(() => childrenApi.group(swarmId, sequence), [swarmId, sequence]);
  const detail = useResource(load, { headerRefresh: false, trackRefresh: true });
  const [asking, setAsking] = useState(false);
  const [busy, setBusy] = useState(false);
  const [submitted, setSubmitted] = useState<ChildGroup | null>(null);
  const [notice, setNotice] = useState(false);
  const [failure, setFailure] = useState('');
  const [unknown, setUnknown] = useState(false);
  const locked = useRef(false);
  const alive = useRef(true);
  const result = useRef<HTMLParagraphElement>(null);
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
    };
  }, []);
  const refreshDetail = detail.refresh;
  const refresh = useCallback(async () => {
    if (!locked.current) await refreshDetail();
  }, [refreshDetail]);
  useHeaderRefresh(refresh);
  useSwarmTick(
    useCallback(() => {
      if (!detail.loading) void refresh();
    }, [detail.loading, refresh])
  );
  const group = detail.data && detail.data !== submitted ? detail.data : null;
  const disagrees = Boolean(notice && !busy && group && group.state !== 'aborted');
  const eligible = Boolean(
    group && !detail.error && !disagrees && canAbortChildGroup(group, owner)
  );
  useEffect(() => {
    if (!eligible) setAsking(false);
  }, [eligible]);
  useEffect(() => {
    if (notice || failure) result.current?.focus();
  }, [notice, failure]);
  const abort = async () => {
    if (!group || !eligible || locked.current) return;
    locked.current = true;
    setBusy(true);
    setSubmitted(group);
    setNotice(false);
    setFailure('');
    setUnknown(false);
    try {
      await childrenApi.abort(swarmId, sequence, group.revision);
      if (alive.current) setNotice(true);
    } catch (error) {
      if (alive.current) {
        setFailure(errorMessage(error));
        setUnknown(
          !(error instanceof BroodApiError && [400, 401, 403, 404, 409].includes(error.status))
        );
      }
    } finally {
      if (alive.current) await detail.refresh();
      if (alive.current) await changed();
      locked.current = false;
      if (alive.current) setBusy(false);
    }
  };
  return (
    <section
      className={local.detail}
      aria-label={t('swarm_children.group', { sequence, pass: detail.data?.pass ?? sequence })}
    >
      {(notice || failure) && (
        <p
          ref={result}
          tabIndex={-1}
          role={failure || disagrees ? 'alert' : 'status'}
          className={styles.notice}
        >
          {notice && t(disagrees ? 'swarm_children.state_changed' : 'swarm_children.abort_success')}
          {unknown && <span>{t('swarm_children.abort_unknown')} </span>}
          {failure}
        </p>
      )}
      <LoadState
        loading={!group && !detail.error}
        error={!group ? (detail.error ?? '') : ''}
        retry={refresh}
      >
        {group && (
          <>
            {detail.error && (
              <p className={`${styles.notice} ${styles.operationFailure}`} role="alert">
                {detail.error}
              </p>
            )}
            <ChildGroupDetailsView
              group={group}
              owner={owner}
              busy={busy}
              stale={Boolean(detail.error) || disagrees}
              onAbort={() => setAsking(true)}
            />
          </>
        )}
      </LoadState>
      <ConfirmDialog
        open={asking && eligible && !busy}
        title={t('swarm_children.abort_title', { sequence })}
        confirmText={t('swarm_children.abort_confirm')}
        onCancel={() => setAsking(false)}
        onConfirm={() => {
          setAsking(false);
          void abort();
        }}
      >
        <p>{t('swarm_children.abort_description')}</p>
        <p>{t('swarm_children.revision', { revision: group?.revision })}</p>
      </ConfirmDialog>
    </section>
  );
}

export function ChildGroupDetailsView({
  group,
  owner,
  stale,
  busy,
  onAbort,
}: {
  group: ChildGroup;
  owner: boolean;
  stale: boolean;
  busy: boolean;
  onAbort: () => void;
}) {
  const { t, i18n } = useTranslation();
  const eligible = !stale && canAbortChildGroup(group, owner);
  return (
    <div className={local.block} data-child-group={group.parentSequence}>
      <h3>{t('swarm_children.group', { sequence: group.parentSequence, pass: group.pass })}</h3>
      <p className={local.identity}>
        {t('swarm_children.parent', { uid: group.parentUid, incarnation: group.parentIncarnation })}
      </p>
      <p className={local.hint}>{t('swarm_children.instance_hint')}</p>
      <dl className={local.facts}>
        <div>
          <dt>{t('swarm_children.group_state')}</dt>
          <dd data-child-group-state="">{stateName(t, 'group_states', group.state)}</dd>
        </div>
        <div>
          <dt>{t('swarm_children.readiness')}</dt>
          <dd data-child-readiness="">{stateName(t, 'readiness_states', group.readiness)}</dd>
        </div>
      </dl>
      <p className={local.hint}>{t('swarm_children.readiness_help')}</p>
      <div className={local.facts}>
        <span>{policyText(t, group)}</span>
        <span>{t('swarm_children.open', { count: group.open })}</span>
      </div>
      {group.interrupted && <p className={styles.notice}>{t('swarm_children.interrupted')}</p>}
      {group.abortedAt && (
        <p className={local.hint}>
          {t('swarm_children.abort_recorded', {
            user: group.abortedBy,
            time: formatDateTimeValue(group.abortedAt, i18n.language),
          })}
        </p>
      )}
      {group.children.length === 0 ? (
        <p className={local.hint}>{t('swarm_children.no_registered')}</p>
      ) : (
        group.children.map((task) => (
          <ChildTaskRecord
            key={task.id}
            task={task}
            open={group.children.length <= 3 || task.delivery === 'unconfirmed'}
          />
        ))
      )}
      {eligible ? (
        <Button
          variant="danger"
          size="sm"
          data-child-abort=""
          loading={busy}
          aria-haspopup="dialog"
          onClick={onAbort}
        >
          {t('swarm_children.abort')}
        </Button>
      ) : (
        <p className={local.hint}>
          {t(owner ? 'swarm_children.abort_unavailable' : 'swarm_children.read_only')}
        </p>
      )}
    </div>
  );
}

function ChildTaskRecord({ task, open }: { task: ChildTask; open: boolean }) {
  const { t, i18n } = useTranslation();
  return (
    <details className={local.task} open={open}>
      <summary className={local.head}>
        <span className={local.identity}>{task.role || task.id}</span>
        <span data-child-task-state="">
          <StatusBadge
            tone={
              task.state === 'running'
                ? 'progress'
                : task.outcome === 'fail'
                  ? 'problem'
                  : 'neutral'
            }
          >
            {stateName(t, 'child_states', task.state)}
          </StatusBadge>
        </span>
      </summary>
      <div className={local.taskBody}>
        <p className={local.identity}>
          {t('swarm_children.executor', {
            uid: task.executorUid,
            incarnation: task.executorIncarnation,
          })}
        </p>
        <p className={local.hint}>
          {t('swarm_children.created', {
            time: formatDateTimeValue(task.createdAt, i18n.language),
          })}
          {task.finishedAt &&
            `; ${t('swarm_children.finished', { time: formatDateTimeValue(task.finishedAt, i18n.language) })}`}
        </p>
        <dl className={local.facts}>
          <div>
            <dt>{t('swarm_children.delivery')}</dt>
            <dd>
              {task.delivery
                ? stateName(t, 'delivery_states', task.delivery)
                : t('swarm_children.delivery_unknown')}
            </dd>
          </div>
          {task.outcome && (
            <div>
              <dt>{t('swarm_children.outcome')}</dt>
              <dd>{stateName(t, 'outcomes', task.outcome)}</dd>
            </div>
          )}
        </dl>
        {task.deliveryDetail && <PlainText text={task.deliveryDetail} className={local.plain} />}
        <h4>{t('swarm_children.task')}</h4>
        <PlainText text={task.task} className={local.plain} />
        <h4>{t('swarm_children.report')}</h4>
        {task.report ? (
          <PlainText text={task.report} className={local.plain} />
        ) : (
          <p className={local.hint}>{t('swarm_children.no_report')}</p>
        )}
      </div>
    </details>
  );
}
