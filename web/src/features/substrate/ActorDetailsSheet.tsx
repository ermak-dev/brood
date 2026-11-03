import { useId, useRef, useState, type KeyboardEvent } from 'react';
import { useTranslation } from 'react-i18next';
import type { Actor } from '@/services/brood/substrate';
import { sessionReadable, type ManagedActionId } from './actorActions';
import type { ActionFailure, UnknownOutcome } from './actorOperations';
import { ActorActionFailure } from './components/ActorActionFailure';
import { ActorHistory } from './components/ActorHistory';
import { ActorMessage } from './components/ActorMessage';
import { ActorOutcomeNotices } from './components/ActorOutcomeNotices';
import { ActorOutput } from './components/ActorOutput';
import { ActorOverview } from './components/ActorOverview';
import { ActorUsage } from './components/ActorUsage';
import { EditorSheet } from './components/EditorSheet';
import { ManagedActorActions } from './components/ManagedActorActions';
import { StateBadge } from './components/StateBadge';
import styles from './substrate.module.scss';

const TABS = ['overview', 'output', 'history', 'usage', 'send'] as const;
type Tab = (typeof TABS)[number];

/** Вкладки по образцу WAI-ARIA tabs: стрелки, Home/End, один tab stop. */
function ConsoleTabs({
  tabs,
  active,
  ids,
  label,
  onChange,
}: {
  tabs: readonly Tab[];
  active: Tab;
  ids: (tab: Tab) => { tab: string; panel: string };
  label: string;
  onChange: (tab: Tab) => void;
}) {
  const { t } = useTranslation();
  const refs = useRef<Partial<Record<Tab, HTMLButtonElement | null>>>({});
  const onKey = (event: KeyboardEvent) => {
    const index = tabs.indexOf(active);
    const next =
      event.key === 'ArrowRight'
        ? (index + 1) % tabs.length
        : event.key === 'ArrowLeft'
          ? (index - 1 + tabs.length) % tabs.length
          : event.key === 'Home'
            ? 0
            : event.key === 'End'
              ? tabs.length - 1
              : -1;
    if (next < 0) return;
    event.preventDefault();
    onChange(tabs[next]);
    refs.current[tabs[next]]?.focus();
  };
  return (
    <div role="tablist" aria-label={label} className={styles.consoleTabs}>
      {tabs.map((tab) => (
        <button
          key={tab}
          ref={(node) => {
            refs.current[tab] = node;
          }}
          type="button"
          role="tab"
          id={ids(tab).tab}
          aria-selected={tab === active}
          aria-controls={ids(tab).panel}
          tabIndex={tab === active ? 0 : -1}
          className={`${styles.consoleTab} ${tab === active ? styles.consoleTabActive : ''}`}
          onClick={() => onChange(tab)}
          onKeyDown={onKey}
        >
          {t(`actors.console.tabs.${tab}`)}
        </button>
      ))}
    </div>
  );
}

/** brood: детали managed-актора — пульт: действия, обзор, вывод, история,
 * расход, сообщение. Данные сессии требуют прав owner/editor роя (сервер
 * проверяет их сам), поэтому viewer видит только обзор. Посещённые вкладки
 * остаются смонтированными: возврат не перечитывает их без запроса. После
 * reset (новое поколение) вкладки сессии читаются заново. */
export function ActorDetailsSheet({
  open,
  onClose,
  actor,
  swarm,
  swarmName,
  canManage,
  administrator,
  pending,
  failure,
  unknown,
  onAction,
  onClearFailure,
  onDismissUnknown,
}: {
  open: boolean;
  onClose: () => void;
  actor: Actor;
  swarm: string;
  swarmName: string;
  canManage: boolean;
  administrator: boolean;
  pending?: ManagedActionId;
  failure: ActionFailure | null;
  unknown: UnknownOutcome[];
  onAction: (id: ManagedActionId) => void;
  onClearFailure: () => void;
  onDismissUnknown: (item: UnknownOutcome) => void;
}) {
  const { t } = useTranslation();
  const base = useId();
  const name = actor.metadata.name;
  const tabs: readonly Tab[] = canManage ? TABS : ['overview'];
  const [tab, setTab] = useState<Tab>('overview');
  const [visited, setVisited] = useState<ReadonlySet<Tab>>(() => new Set<Tab>(['overview']));
  const select = (next: Tab) => {
    setTab(next);
    setVisited((previous) => new Set(previous).add(next));
  };
  const ids = (id: Tab) => ({ tab: `${base}-tab-${id}`, panel: `${base}-panel-${id}` });
  const readable = sessionReadable(actor);
  const generation = `${actor.metadata.uid ?? name}:${actor.generation ?? ''}`;

  const panel = (id: Tab) => {
    if (id === 'overview') return <ActorOverview actor={actor} />;
    if (!readable) return <p className={styles.notice}>{t('actors.console.not_running')}</p>;
    if (id === 'output') return <ActorOutput swarm={swarm} name={name} />;
    if (id === 'history') return <ActorHistory swarm={swarm} name={name} />;
    if (id === 'usage') return <ActorUsage swarm={swarm} name={name} />;
    return <ActorMessage swarm={swarm} name={name} />;
  };

  return (
    <EditorSheet
      open={open}
      onClose={onClose}
      size="lg"
      eyebrow={swarmName}
      title={t('actors.details_title', { name })}
    >
      <div className={styles.form}>
        <div className={styles.consoleHead}>
          <StateBadge state={actor.phase ?? ''} namespace="actors.managed_state" />
          {canManage && (
            <div className={styles.actorActionsWrap}>
              <ManagedActorActions
                actor={actor}
                swarm={swarm}
                canManage={canManage}
                pending={pending}
                onAction={onAction}
              />
            </div>
          )}
        </div>
        {failure?.name === name && (
          <ActorActionFailure
            name={name}
            action={failure.action ? t(`actors.ops.${failure.action}.label`) : undefined}
            error={failure.error}
            administrator={administrator}
            onClose={onClearFailure}
          />
        )}
        <ActorOutcomeNotices
          items={unknown.filter((item) => item.name === name)}
          onDismiss={onDismissUnknown}
        />
        {canManage ? (
          <ConsoleTabs
            tabs={tabs}
            active={tab}
            ids={ids}
            label={t('actors.console.tabs_label', { name })}
            onChange={select}
          />
        ) : (
          <p className={styles.notice}>{t('actors.console.viewer_hint')}</p>
        )}
        {/* Контейнер каждой вкладки есть всегда (aria-controls указывает на
            существующий элемент); содержимое монтируется при первом открытии. */}
        {tabs.map((id) => (
          <div
            key={`${id}:${generation}`}
            role={canManage ? 'tabpanel' : undefined}
            id={ids(id).panel}
            aria-labelledby={canManage ? ids(id).tab : undefined}
            hidden={tab !== id}
          >
            {visited.has(id) && panel(id)}
          </div>
        ))}
      </div>
    </EditorSheet>
  );
}
