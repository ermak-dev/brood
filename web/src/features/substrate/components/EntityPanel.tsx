import { useId, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import type { SheetSize } from '@/components/ui/Sheet';
import { EditorSheet } from './EditorSheet';
import styles from '../substrate.module.scss';

export interface PanelAction {
  id: string;
  label: string;
  /** Что делает действие, простыми словами; видно под названием. */
  description?: string;
  /** Почему действие сейчас недоступно; есть — пункт заблокирован, причина видна. */
  blocked?: string;
  /** primary — главное действие объекта; danger — необратимое. */
  tone?: 'primary' | 'danger';
  /** Действие идёт, кнопка ждёт его исхода. */
  pending?: boolean;
  /** Ссылка вместо действия: открывается в новой вкладке (терминал). */
  href?: string;
  onSelect?: () => void;
}

export interface PanelActionGroup {
  id: string;
  label: string;
  /** Необратимые действия: отдельный блок внизу панели. */
  danger?: boolean;
  actions: PanelAction[];
}

/** brood: боковая панель объекта, единая для страниц Проекты..Пулы воркеров.
 * Строка таблицы (карточка на телефоне) только открывает её; все действия над
 * объектом живут здесь: сначала статус и обычные группы крупными кнопками с
 * пояснением, затем сведения, в конце — необратимые отдельно. На телефоне
 * панель во всю ширину, кнопки в столбик под палец. Недоступное действие
 * остаётся в фокусе и называет причину словами (aria-disabled, не disabled). */
export function EntityPanel({
  open,
  onClose,
  title,
  eyebrow,
  description,
  status,
  size = 'lg',
  actions = [],
  children,
}: {
  open: boolean;
  onClose: () => void;
  title: ReactNode;
  eyebrow?: ReactNode;
  description?: ReactNode;
  /** Короткая сводка состояния над действиями (бейдж, место размещения). */
  status?: ReactNode;
  size?: SheetSize;
  actions?: PanelActionGroup[];
  children?: ReactNode;
}) {
  const visible = actions.filter((group) => group.actions.length > 0);
  return (
    <EditorSheet open={open} onClose={onClose} size={size} eyebrow={eyebrow} title={title} description={description}>
      <div className={styles.panel}>
        {status && <div className={styles.panelStatus}>{status}</div>}
        {visible.filter((group) => !group.danger).map((group) => (
          <PanelGroup key={group.id} group={group} />
        ))}
        {children}
        {visible.filter((group) => group.danger).map((group) => (
          <PanelGroup key={group.id} group={group} />
        ))}
      </div>
    </EditorSheet>
  );
}

/** Группы действий в том же виде, что и в EntityPanel, — для панели, которая
 * собрана сама (вкладки, форма редактора). Порядок — как передан; пустые
 * группы не показываются. */
export function PanelActions({ groups }: { groups: PanelActionGroup[] }) {
  return (
    <>
      {groups
        .filter((group) => group.actions.length > 0)
        .map((group) => (
          <PanelGroup key={group.id} group={group} />
        ))}
    </>
  );
}

function PanelGroup({ group }: { group: PanelActionGroup }) {
  const labelId = useId();
  return (
    <section
      className={[styles.panelGroup, group.danger ? styles.panelGroupDanger : ''].filter(Boolean).join(' ')}
      aria-labelledby={labelId}
    >
      <h3 id={labelId} className={styles.panelGroupLabel}>
        {group.label}
      </h3>
      <div className={styles.panelActions}>
        {group.actions.map((action) => (
          <PanelButton key={action.id} action={action} />
        ))}
      </div>
    </section>
  );
}

function PanelButton({ action }: { action: PanelAction }) {
  const { t } = useTranslation();
  // Имя кнопки — её заголовок, описание — отдельно (#256): без этого
  // скринридер читал карточку целиком как имя, а потом ещё раз как описание.
  const labelId = useId();
  const newTabId = useId();
  const descriptionId = useId();
  const unavailable = Boolean(action.blocked) || Boolean(action.pending);
  const className = [
    styles.panelAction,
    action.tone === 'primary' ? styles.panelActionPrimary : '',
    action.tone === 'danger' ? styles.panelActionDanger : '',
    unavailable ? styles.panelActionBlocked : '',
  ]
    .filter(Boolean)
    .join(' ');
  const body = (
    <>
      <span id={labelId} className={styles.panelActionLabel}>
        {action.pending && <span className="loading-spinner" aria-hidden="true" />}
        {action.label}
      </span>
      {(action.description || action.blocked) && (
        <span id={descriptionId} className={styles.panelActionText}>
          {action.description}
          {action.blocked && <span className={styles.menuItemReason}>{action.blocked}</span>}
        </span>
      )}
    </>
  );
  const described = action.description || action.blocked ? descriptionId : undefined;
  if (action.href && !unavailable) {
    return (
      <a
        className={className}
        href={action.href}
        target="_blank"
        rel="noopener noreferrer"
        aria-labelledby={`${labelId} ${newTabId}`}
        aria-describedby={described}
      >
        {body}
        <span id={newTabId} className={styles.srOnly}>
          {t('substrate.opens_new_tab')}
        </span>
      </a>
    );
  }
  return (
    <button
      type="button"
      className={className}
      aria-disabled={unavailable ? true : undefined}
      aria-busy={action.pending ? true : undefined}
      aria-labelledby={labelId}
      aria-describedby={described}
      onClick={unavailable ? undefined : action.onSelect}
    >
      {body}
    </button>
  );
}
