import { useCallback, useId, useLayoutEffect, useRef, type ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { Sheet } from '@/components/ui/Sheet';
import type { Icon } from '../ui';
import styles from './table.module.scss';

/** What an object offers in its panel. Exactly one of `href` (a link in a
 * new tab), `to` (another page of the panel), `onSelect` (a sheet of the
 * page) or `run` (a mutation, asked about first when `confirm` is set). */
export interface PanelAction {
  id: string;
  label: string;
  icon: Icon;
  /** What happens, in one plain line under the label. */
  description: string;
  /** Why it cannot be used now: the action stays visible and says so. */
  blocked?: string;
  /** primary: what people open the object for; danger: irreversible. */
  tone?: 'primary' | 'danger';
  href?: string;
  to?: string;
  onSelect?: () => void;
  run?: () => Promise<unknown>;
  /** Names the object and the consequence; asked before `run`. */
  confirm?: string;
  /** The notification after `run`; by default it says the action is done.
   * An action the server only starts says what was requested instead. */
  notice?: string;
}

export interface PanelGroup {
  id: string;
  label: string;
  /** Irreversible actions: the separate block at the bottom of the panel. */
  danger?: boolean;
  actions: PanelAction[];
}

export interface PanelSpec {
  icon: Icon;
  title: string;
  /** The state badge next to the name. */
  badge?: ReactNode;
  /** The status summary above the actions. */
  status?: ReactNode;
  groups: PanelGroup[];
  /** Details of the object, between the ordinary and the dangerous. */
  details?: ReactNode;
}

/** The side panel of an object: a row of the table (a card on a phone) only
 * opens it, and everything about the object lives here. Status on top, the
 * ordinary actions as large buttons with a one-line description, then the
 * details, and the irreversible actions in a block of their own at the
 * bottom. An unavailable action stays in the tab order and names its
 * reason in words (aria-disabled, not disabled). */
export function EntityPanel({
  open,
  close,
  spec,
  pending,
  busy,
  select,
}: {
  open: boolean;
  close: () => void;
  spec: PanelSpec | null;
  /** The id of the action whose mutation is in flight. */
  pending: string | null;
  /** Shown instead of the reason while another action runs. */
  busy: string;
  select: (action: PanelAction) => void;
}) {
  const { t } = useTranslation();
  // One stable onClose: the Sheet restarts its closing when it changes.
  const closeRef = useRef(close);
  useLayoutEffect(() => {
    closeRef.current = close;
  });
  const stableClose = useCallback(() => closeRef.current(), []);
  if (!spec) return null;
  const Mark = spec.icon;
  const groups = spec.groups.filter((group) => group.actions.length > 0);
  const render = (group: PanelGroup) => (
    <ActionGroup key={group.id} group={group} pending={pending} busy={busy} select={select} />
  );
  return (
    <Sheet
      open={open}
      onClose={stableClose}
      size="md"
      title={
        <span className={styles.sheetTitle}>
          <Mark size={18} />
          <span className={styles.sheetName}>{spec.title}</span>
          {spec.badge}
        </span>
      }
      footer={
        <Button variant="secondary" onClick={stableClose}>
          {t('common.close')}
        </Button>
      }
    >
      <div className={styles.panel}>
        {spec.status && <div className={styles.panelStatus}>{spec.status}</div>}
        {groups.filter((group) => !group.danger).map(render)}
        {spec.details}
        {groups.filter((group) => group.danger).map(render)}
      </div>
    </Sheet>
  );
}

/** A titled block of the panel: details, images, commands. */
export function PanelSection({ title, children }: { title: string; children: ReactNode }) {
  const labelId = useId();
  return (
    <section className={styles.panelGroup} aria-labelledby={labelId}>
      <h3 id={labelId} className={styles.panelGroupLabel}>
        {title}
      </h3>
      {children}
    </section>
  );
}

function ActionGroup({
  group,
  pending,
  busy,
  select,
}: {
  group: PanelGroup;
  pending: string | null;
  busy: string;
  select: (action: PanelAction) => void;
}) {
  const labelId = useId();
  return (
    <section
      className={`${styles.panelGroup} ${group.danger ? styles.panelGroupDanger : ''}`}
      aria-labelledby={labelId}
    >
      <h3 id={labelId} className={styles.panelGroupLabel}>
        {group.label}
      </h3>
      <div className={styles.panelActions}>
        {group.actions.map((action) => (
          <ActionButton
            key={action.id}
            action={
              pending !== null && pending !== action.id && !action.blocked
                ? { ...action, blocked: busy }
                : action
            }
            running={pending === action.id}
            select={select}
          />
        ))}
      </div>
    </section>
  );
}

function ActionButton({
  action,
  running,
  select,
}: {
  action: PanelAction;
  running: boolean;
  select: (action: PanelAction) => void;
}) {
  const { t } = useTranslation();
  // The name of the button is its label; the description and the reason
  // are its description, so a screen reader does not read the card twice.
  const labelId = useId();
  const textId = useId();
  const Mark = action.icon;
  const unavailable = Boolean(action.blocked) || running;
  const className = [
    styles.panelAction,
    action.tone === 'primary' && styles.panelActionPrimary,
    action.tone === 'danger' && styles.panelActionDanger,
    unavailable && styles.panelActionBlocked,
  ]
    .filter(Boolean)
    .join(' ');
  const body = (
    <>
      <span className={styles.panelActionIcon}>
        {running ? <span className="loading-spinner" aria-hidden="true" /> : <Mark size={18} />}
      </span>
      <span className={styles.panelActionCopy}>
        <span id={labelId} className={styles.panelActionLabel}>
          {action.label}
        </span>
        <span id={textId} className={styles.panelActionText}>
          {action.description}
          {action.blocked && <span className={styles.panelActionReason}>{action.blocked}</span>}
        </span>
      </span>
    </>
  );
  if (action.href && !unavailable)
    return (
      <a
        className={className}
        href={action.href}
        target="_blank"
        rel="noopener noreferrer"
        aria-labelledby={labelId}
        aria-describedby={textId}
      >
        {body}
        <span className={styles.srOnly}>{t('brood.panel.new_tab')}</span>
      </a>
    );
  if (action.to && !unavailable)
    return (
      <Link className={className} to={action.to} aria-labelledby={labelId} aria-describedby={textId}>
        {body}
      </Link>
    );
  return (
    <button
      type="button"
      className={className}
      aria-disabled={unavailable ? true : undefined}
      aria-busy={running ? true : undefined}
      aria-labelledby={labelId}
      aria-describedby={textId}
      onClick={unavailable ? undefined : () => select(action)}
    >
      {body}
    </button>
  );
}
