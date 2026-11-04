import { useId, useRef, useState, type KeyboardEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { actorsApi, type Actor } from '@/services/brood/substrate';
import { managedActionGroups, type ManagedActionId } from './actorActions';
import type { ActionFailure, UnknownOutcome } from './actorOperations';
import { ActorActionFailure } from './components/ActorActionFailure';
import { ActorOutcomeNotices } from './components/ActorOutcomeNotices';
import { ActorOverview } from './components/ActorOverview';
import { EntityPanel, PanelActions } from './components/EntityPanel';
import { StateBadge } from './components/StateBadge';
import styles from './substrate.module.scss';

const TABS = ['overview', 'actions'] as const;
type Tab = (typeof TABS)[number];

/** Стрелки и Home/End меняют вкладку; в Tab-порядке только активная. */
function ActorTabs({
  active,
  ids,
  label,
  onChange,
}: {
  active: Tab;
  ids: (tab: Tab) => { tab: string; panel: string };
  label: string;
  onChange: (tab: Tab) => void;
}) {
  const { t } = useTranslation();
  const refs = useRef<Partial<Record<Tab, HTMLButtonElement | null>>>({});
  const onKey = (event: KeyboardEvent) => {
    const index = TABS.indexOf(active);
    const next =
      event.key === 'ArrowRight'
        ? (index + 1) % TABS.length
        : event.key === 'ArrowLeft'
          ? (index - 1 + TABS.length) % TABS.length
          : event.key === 'Home'
            ? 0
            : event.key === 'End'
              ? TABS.length - 1
              : -1;
    if (next < 0) return;
    event.preventDefault();
    onChange(TABS[next]);
    refs.current[TABS[next]]?.focus();
  };
  return (
    <div role="tablist" aria-label={label} className={styles.consoleTabs}>
      {TABS.map((tab) => (
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

/** Сведения и операции разделены; ошибки и неизвестные исходы видны на обеих
 * вкладках. Viewer получает только сведения, без изменяющих действий. */
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
  const [tab, setTab] = useState<Tab>('overview');
  const ids = (id: Tab) => ({ tab: `${base}-tab-${id}`, panel: `${base}-panel-${id}` });
  const groups = managedActionGroups(actor, {
    t,
    canManage,
    pending,
    terminal: actorsApi.terminal(swarm, name),
    onAction,
  }).map((group) => ({
    ...group,
    actions: group.actions.filter((action) => action.id !== 'terminal'),
  }));

  return (
    <EntityPanel
      open={open}
      onClose={onClose}
      eyebrow={swarmName}
      title={t('actors.details_title', { name })}
      status={
        <>
          <StateBadge state={actor.phase ?? ''} namespace="actors.managed_state" />
          <span role="status" className={pending ? styles.pendingNote : undefined}>
            {pending && (
              <>
                <span className="loading-spinner" aria-hidden="true" />
                {t(`actors.ops.${pending}.pending`, { name })}
              </>
            )}
          </span>
        </>
      }
    >
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
      {canManage && (
        <ActorTabs
          active={tab}
          ids={ids}
          label={t('actors.console.tabs_label', { name })}
          onChange={setTab}
        />
      )}
      <div
        role={canManage ? 'tabpanel' : undefined}
        id={ids('overview').panel}
        aria-labelledby={canManage ? ids('overview').tab : undefined}
        hidden={canManage && tab !== 'overview'}
      >
        <ActorOverview actor={actor} />
      </div>
      {canManage && (
        <div
          role="tabpanel"
          id={ids('actions').panel}
          aria-labelledby={ids('actions').tab}
          hidden={tab !== 'actions'}
        >
          <div className={styles.panel}>
            <PanelActions groups={groups} />
          </div>
        </div>
      )}
    </EntityPanel>
  );
}
