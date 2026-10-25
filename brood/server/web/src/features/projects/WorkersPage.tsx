import { useCallback, useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { EmptyState } from '@/components/ui/EmptyState';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/Table';
import { useNotificationStore } from '@/stores';
import { useHeaderRefresh } from '@/hooks/useHeaderRefresh';
import { drainWorker, listWorkers, type BroodWorker } from '@/services/brood/projects';
import styles from './projects.module.scss';

/** brood: воркеры установки -- зеркала подов atelet, на которых живут
 * акторы. Drain помечает воркер завершающимся: планировщик больше не
 * сажает на него акторов. */
export function WorkersPage() {
  const { t } = useTranslation();
  const showNotification = useNotificationStore((state) => state.showNotification);
  const showConfirmation = useNotificationStore((state) => state.showConfirmation);
  const [workers, setWorkers] = useState<BroodWorker[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    try {
      setWorkers(await listWorkers());
      setError(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  useHeaderRefresh(refresh);

  const drain = useCallback(
    (worker: BroodWorker) => {
      showConfirmation({
        title: t('workers.drain_title'),
        message: t('workers.drain_message', { name: worker.name }),
        variant: 'danger',
        confirmText: t('workers.drain'),
        onConfirm: async () => {
          try {
            await drainWorker(worker.name);
            showNotification(t('workers.drained', { name: worker.name }), 'success');
            await refresh();
          } catch (e) {
            showNotification(e instanceof Error ? e.message : String(e), 'error');
          }
        },
      });
    },
    [refresh, showConfirmation, showNotification, t]
  );

  return (
    <div className={styles.page}>
      <div className={styles.header}>
        <div>
          <h1 className={styles.title}>{t('workers.title')}</h1>
          <p className={styles.description}>{t('workers.description')}</p>
        </div>
      </div>

      {workers === null && !error ? (
        <LoadingSpinner />
      ) : error ? (
        <EmptyState title={t('projects.load_failed')} description={error} />
      ) : workers && workers.length > 0 ? (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{t('projects.name')}</TableHead>
              <TableHead>{t('projects.state')}</TableHead>
              <TableHead>{t('workers.pool')}</TableHead>
              <TableHead>{t('workers.node')}</TableHead>
              <TableHead>{t('workers.actors')}</TableHead>
              <TableHead />
            </TableRow>
          </TableHeader>
          <TableBody>
            {workers.map((worker) => (
              <TableRow key={worker.uid}>
                <TableCell>{worker.name}</TableCell>
                <TableCell>
                  <span className={`status-badge ${worker.state === 'active' ? 'success' : 'warning'}`}>
                    {t(`workers.state.${worker.state}`, worker.state)}
                  </span>
                </TableCell>
                <TableCell>{worker.worker_pool || '—'}</TableCell>
                <TableCell>{worker.node || '—'}</TableCell>
                <TableCell>
                  {worker.actors_allocated}/{worker.actors_capacity}
                </TableCell>
                <TableCell>
                  {worker.state === 'active' && (
                    <Button variant="danger" size="sm" onClick={() => drain(worker)}>
                      {t('workers.drain')}
                    </Button>
                  )}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      ) : (
        <EmptyState
          title={t('workers.empty')}
          description={t('workers.empty_description')}
        />
      )}
    </div>
  );
}
