import { useCallback, useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { EmptyState } from '@/components/ui/EmptyState';
import { Input } from '@/components/ui/Input';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';
import { Modal } from '@/components/ui/Modal';
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
import { formatDateValue } from '@/utils/format';
import {
  createProject,
  deleteProject,
  listProjects,
  ProjectsApiError,
  type BroodProject,
} from '@/services/brood/projects';
import styles from './projects.module.scss';

const DASH = '—';

/** brood: проекты -- atespace'ы Substrate установки. CRUD через /api/projects
 * сервера brood; удаление непустой atespace отказывает на стороне Substrate,
 * причина показывается как есть. */
export function ProjectsPage() {
  const { t, i18n } = useTranslation();
  const showNotification = useNotificationStore((state) => state.showNotification);
  const showConfirmation = useNotificationStore((state) => state.showConfirmation);
  const [projects, setProjects] = useState<BroodProject[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);
  const [name, setName] = useState('');

  const refresh = useCallback(async () => {
    try {
      setProjects(await listProjects());
      setError(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  useHeaderRefresh(refresh);

  const submit = useCallback(async () => {
    const trimmed = name.trim();
    if (!trimmed) return;
    try {
      await createProject(trimmed);
      showNotification(t('projects.created', { name: trimmed }), 'success');
      setName('');
      setCreating(false);
      await refresh();
    } catch (e) {
      showNotification(e instanceof Error ? e.message : String(e), 'error');
    }
  }, [name, refresh, showNotification, t]);

  const remove = useCallback(
    (project: BroodProject) => {
      showConfirmation({
        title: t('projects.delete_title'),
        message: t('projects.delete_message', { name: project.name }),
        variant: 'danger',
        confirmText: t('projects.delete_confirm'),
        onConfirm: async () => {
          try {
            await deleteProject(project.name);
            showNotification(t('projects.deleted', { name: project.name }), 'success');
            await refresh();
          } catch (e) {
            // 409 FAILED_PRECONDITION: atespace не пуста, причина от сервера.
            showNotification(
              e instanceof ProjectsApiError && e.status === 409
                ? t('projects.delete_failed_not_empty', { detail: e.message })
                : e instanceof Error
                  ? e.message
                  : String(e),
              'error'
            );
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
          <h1 className={styles.title}>{t('projects.title')}</h1>
          <p className={styles.description}>{t('projects.description')}</p>
        </div>
        <div className={styles.actions}>
          <Button onClick={() => setCreating(true)}>{t('projects.create')}</Button>
        </div>
      </div>

      {projects === null && !error ? (
        <LoadingSpinner />
      ) : error ? (
        <EmptyState title={t('projects.load_failed')} description={error} />
      ) : projects && projects.length > 0 ? (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{t('projects.name')}</TableHead>
              <TableHead>{t('projects.created_at')}</TableHead>
              <TableHead>{t('projects.updated_at')}</TableHead>
              <TableHead />
            </TableRow>
          </TableHeader>
          <TableBody>
            {projects.map((project) => (
              <TableRow key={project.uid}>
                <TableCell>{project.name}</TableCell>
                <TableCell>
                  {project.create_time
                    ? formatDateValue(project.create_time, i18n.language)
                    : DASH}
                </TableCell>
                <TableCell>
                  {project.update_time
                    ? formatDateValue(project.update_time, i18n.language)
                    : DASH}
                </TableCell>
                <TableCell>
                  <Button variant="danger" size="sm" onClick={() => remove(project)}>
                    {t('projects.delete')}
                  </Button>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      ) : (
        <EmptyState
          title={t('projects.empty')}
          description={t('projects.empty_description')}
          action={<Button onClick={() => setCreating(true)}>{t('projects.create')}</Button>}
        />
      )}

      <Modal
        open={creating}
        title={t('projects.create_title')}
        onClose={() => setCreating(false)}
        footer={
          <>
            <Button variant="secondary" onClick={() => setCreating(false)}>
              {t('common.cancel')}
            </Button>
            <Button onClick={() => void submit()} disabled={!name.trim()}>
              {t('projects.create')}
            </Button>
          </>
        }
      >
        <Input
          name="project-name"
          label={t('projects.name_label')}
          value={name}
          placeholder={t('projects.name_placeholder')}
          onChange={(event) => setName(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === 'Enter' && name.trim()) void submit();
          }}
        />
      </Modal>
    </div>
  );
}
