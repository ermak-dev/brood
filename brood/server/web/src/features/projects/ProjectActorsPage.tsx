import { useCallback, useEffect, useState } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
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
import { useNotificationStore, useProjectStore } from '@/stores';
import { useHeaderRefresh } from '@/hooks/useHeaderRefresh';
import { formatDateValue } from '@/utils/format';
import {
  actorAction,
  deleteActor,
  listActors,
  type BroodActor,
} from '@/services/brood/projects';
import styles from './projects.module.scss';

const DASH = '—';

/** brood: акторы проекта -- Actor'ы Substrate его atespace. Suspend/resume --
 * снапшот и пробуждение, сердце мультиплексирования; действия идут через
 * /api/projects/<project>/actors сервера brood. Проект -- из URL (ссылка
 * со страницы проектов) или глобальный переключатель. */
export function ProjectActorsPage() {
  const params = useParams<{ project?: string }>();
  const storeProject = useProjectStore((state) => state.project);
  const project = params.project || storeProject;
  const navigate = useNavigate();
  const { t, i18n } = useTranslation();
  const showNotification = useNotificationStore((state) => state.showNotification);
  const showConfirmation = useNotificationStore((state) => state.showConfirmation);
  const [actors, setActors] = useState<BroodActor[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    try {
      setActors(await listActors(project));
      setError(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }, [project]);

  useEffect(() => {
    if (project) void refresh();
    else setActors(null);
  }, [project, refresh]);

  useHeaderRefresh(refresh);

  const act = useCallback(
    async (actor: BroodActor, action: 'suspend' | 'resume') => {
      try {
        const updated = await actorAction(project, actor.name, action);
        showNotification(
          t(`projects.actor_${action}ed`, { name: updated.name }),
          'success'
        );
        await refresh();
      } catch (e) {
        showNotification(e instanceof Error ? e.message : String(e), 'error');
      }
    },
    [project, refresh, showNotification, t]
  );

  const remove = useCallback(
    (actor: BroodActor) => {
      showConfirmation({
        title: t('projects.actor_delete_title'),
        message: t('projects.actor_delete_message', { name: actor.name }),
        variant: 'danger',
        confirmText: t('projects.delete_confirm'),
        onConfirm: async () => {
          try {
            await deleteActor(project, actor.name);
            showNotification(t('projects.actor_deleted', { name: actor.name }), 'success');
            await refresh();
          } catch (e) {
            showNotification(e instanceof Error ? e.message : String(e), 'error');
          }
        },
      });
    },
    [project, refresh, showConfirmation, showNotification, t]
  );

  return (
    <div className={styles.page}>
      <div className={styles.header}>
        <div>
          <p className={styles.description}>
            <Link to="/projects">{t('projects.title')}</Link> / {project}
          </p>
          <h1 className={styles.title}>{t('projects.actors_title', { project })}</h1>
          <p className={styles.description}>{t('projects.actors_description')}</p>
        </div>
        <div className={styles.actions}>
          <Button variant="secondary" onClick={() => navigate('/projects')}>
            {t('common.back')}
          </Button>
        </div>
      </div>

      {actors === null && !error ? (
        <LoadingSpinner />
      ) : error ? (
        <EmptyState title={t('projects.load_failed')} description={error} />
      ) : actors && actors.length > 0 ? (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{t('projects.name')}</TableHead>
              <TableHead>{t('projects.state')}</TableHead>
              <TableHead>{t('projects.created_at')}</TableHead>
              <TableHead>{t('projects.updated_at')}</TableHead>
              <TableHead />
            </TableRow>
          </TableHeader>
          <TableBody>
            {actors.map((actor) => (
              <TableRow key={actor.uid}>
                <TableCell>{actor.name}</TableCell>
                <TableCell>{t(`projects.actor_state.${actor.state}`, actor.state)}</TableCell>
                <TableCell>
                  {actor.create_time ? formatDateValue(actor.create_time, i18n.language) : DASH}
                </TableCell>
                <TableCell>
                  {actor.update_time ? formatDateValue(actor.update_time, i18n.language) : DASH}
                </TableCell>
                <TableCell>
                  <div className={styles.rowActions}>
                    {actor.state === 'running' && (
                      <Button
                        size="sm"
                        onClick={() => void act(actor, 'suspend')}
                      >
                        {t('projects.actor_suspend')}
                      </Button>
                    )}
                    {actor.state === 'suspended' && (
                      <Button size="sm" onClick={() => void act(actor, 'resume')}>
                        {t('projects.actor_resume')}
                      </Button>
                    )}
                    <Button variant="danger" size="sm" onClick={() => remove(actor)}>
                      {t('projects.delete')}
                    </Button>
                  </div>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      ) : (
        <EmptyState
          title={project ? t('projects.actors_empty') : t('projects.none_selected')}
          description={
            project
              ? t('projects.actors_empty_description')
              : t('projects.none_selected_description')
          }
        />
      )}
    </div>
  );
}
