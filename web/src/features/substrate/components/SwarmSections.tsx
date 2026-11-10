import {
  createContext,
  Fragment,
  useCallback,
  useContext,
  useEffect,
  useState,
  type ReactNode,
} from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom';
import { accessApi, type AccessProject, type AccessSwarm } from '@/services/brood/access';
import { Button } from '@/components/ui/Button';
import { EmptyState } from '@/components/ui/EmptyState';
import { IconChevronRight } from '@/components/ui/icons';
import { LoadState } from '@/features/access/shared';
import { useResource } from '../hooks';
import { PageHeader } from './PageHeader';
import styles from '../substrate.module.scss';

interface SectionFailure {
  name: string;
  error: string;
}

/** Отказы загрузки групп, собранные в плашки: один текст -- одна плашка с
 * именами роёв, которых он коснулся, в порядке первого появления. */
export function groupFailures(
  failures: SectionFailure[]
): Array<{ error: string; names: string[] }> {
  const grouped: Array<{ error: string; names: string[] }> = [];
  for (const failure of failures) {
    const notice = grouped.find((item) => item.error === failure.error);
    if (notice) notice.names.push(failure.name);
    else grouped.push({ error: failure.error, names: [failure.name] });
  }
  return grouped;
}

type Report = (swarmId: string, name: string, error: string | null) => void;

const FailureContext = createContext<Report>(() => {});

/** Группа сообщает свой отказ загрузки наверх: SwarmSections показывает
 * одну плашку на причину вместо плашки под каждым роем (#255). */
export function useSwarmSectionFailure(swarmId: string, name: string, error: string | null) {
  const report = useContext(FailureContext);
  useEffect(() => {
    report(swarmId, name, error);
    return () => report(swarmId, name, null);
  }, [report, swarmId, name, error]);
}

/** Строка меты и действия группы -- на месте заголовка страницы: имя роя
 * уже в заголовке сворачиваемой группы. */
export function GroupBar({ meta, actions }: { meta: ReactNode[]; actions?: ReactNode }) {
  const items = meta.filter(Boolean);
  return (
    <div className={styles.header}>
      {items.length > 0 ? (
        <p className={styles.meta}>
          {items.map((item, index) => (
            <Fragment key={index}>
              {index > 0 && (
                <span className={styles.metaDot} aria-hidden="true">
                  ·
                </span>
              )}
              <span>{item}</span>
            </Fragment>
          ))}
        </p>
      ) : (
        <span />
      )}
      {actions && <div className={styles.actions}>{actions}</div>}
    </div>
  );
}

/** Каталог уже отфильтрован сервером: администратору все рои проекта,
 * участнику только собственные. Каждый редактор получает ID своего роя,
 * даже когда в общей странице встречаются одинаковые имена акторов.
 * Группы сворачиваются; отказы групп -- в плашках над ними. */
export function SwarmSections({
  project,
  title,
  children,
}: {
  project: AccessProject;
  title: string;
  children: (swarm: AccessSwarm) => ReactNode;
}) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const load = useCallback(() => accessApi.swarms(project.id), [project.id]);
  const resource = useResource(load);
  const swarms = resource.data?.filter((swarm) => swarm.state === 'active') ?? [];
  const [failures, setFailures] = useState<Record<string, SectionFailure>>({});
  const report = useCallback<Report>((swarmId, name, error) => {
    setFailures((current) => {
      if (error === null) {
        if (!(swarmId in current)) return current;
        const next = { ...current };
        delete next[swarmId];
        return next;
      }
      if (current[swarmId]?.error === error) return current;
      return { ...current, [swarmId]: { name, error } };
    });
  }, []);
  const notices = groupFailures(swarms.map((swarm) => failures[swarm.id]).filter(Boolean));
  return (
    <div className={styles.page}>
      <PageHeader title={title} meta={[project.name]} />
      <LoadState {...resource} error={resource.error ?? ''} retry={resource.refresh}>
        {notices.map((notice) => (
          <p
            key={notice.error}
            role="alert"
            className={`${styles.notice} ${styles.operationFailure}`}
          >
            {t('substrate.load_failed')}: {notice.error}{' '}
            <span className={styles.muted}>
              {t('access.failed_swarms', { names: notice.names.join(', ') })}
            </span>
          </p>
        ))}
        {swarms.length ? (
          <FailureContext.Provider value={report}>
            {swarms.map((swarm) => (
              <details key={swarm.id} className={styles.group} open>
                <summary className={styles.groupSummary}>
                  <IconChevronRight size={16} className={styles.groupChevron} aria-hidden="true" />
                  <h2 className={styles.groupTitle}>{swarm.name}</h2>
                </summary>
                <div className={styles.groupBody}>{children(swarm)}</div>
              </details>
            ))}
          </FailureContext.Provider>
        ) : (
          <EmptyState
            title={t('access.no_active_swarms')}
            action={<Button onClick={() => navigate('/swarms')}>{t('access.swarms')}</Button>}
          />
        )}
      </LoadState>
    </div>
  );
}
