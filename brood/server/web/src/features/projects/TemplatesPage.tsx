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
import { useNotificationStore, useProjectStore } from '@/stores';
import { useHeaderRefresh } from '@/hooks/useHeaderRefresh';
import { formatDateValue } from '@/utils/format';
import { deleteTemplate, listTemplates, type BroodTemplate } from '@/services/brood/projects';
import styles from './projects.module.scss';

const DASH = '—';

/** brood: шаблоны акторов активного проекта. Чтение и удаление; создание --
 * отдельный слой (контейнеры, тома, снапшот-политики). */
export function TemplatesPage() {
  const project = useProjectStore((state) => state.project);
  const { t, i18n } = useTranslation();
  const showNotification = useNotificationStore((state) => state.showNotification);
  const showConfirmation = useNotificationStore((state) => state.showConfirmation);
  const [templates, setTemplates] = useState<BroodTemplate[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    if (!project) return;
    try {
      setTemplates(await listTemplates(project));
      setError(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }, [project]);

  useEffect(() => {
    if (project) void refresh();
    else setTemplates(null);
  }, [project, refresh]);

  useHeaderRefresh(refresh);

  const remove = useCallback(
    (template: BroodTemplate) => {
      showConfirmation({
        title: t('templates.delete_title'),
        message: t('templates.delete_message', { name: template.name }),
        variant: 'danger',
        confirmText: t('projects.delete_confirm'),
        onConfirm: async () => {
          try {
            await deleteTemplate(project, template.name);
            showNotification(t('templates.deleted', { name: template.name }), 'success');
            await refresh();
          } catch (e) {
            showNotification(e instanceof Error ? e.message : String(e), 'error');
          }
        },
      });
    },
    [project, refresh, showConfirmation, showNotification, t]
  );

  if (!project) {
    return (
      <div className={styles.page}>
        <EmptyState
          title={t('projects.none_selected')}
          description={t('projects.none_selected_description')}
        />
      </div>
    );
  }

  return (
    <div className={styles.page}>
      <div className={styles.header}>
        <div>
          <h1 className={styles.title}>{t('templates.title')}</h1>
          <p className={styles.description}>
            {t('templates.description', { project })}
          </p>
        </div>
      </div>

      {templates === null && !error ? (
        <LoadingSpinner />
      ) : error ? (
        <EmptyState title={t('projects.load_failed')} description={error} />
      ) : templates && templates.length > 0 ? (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{t('projects.name')}</TableHead>
              <TableHead>{t('templates.containers')}</TableHead>
              <TableHead>{t('projects.created_at')}</TableHead>
              <TableHead />
            </TableRow>
          </TableHeader>
          <TableBody>
            {templates.map((template) => (
              <TableRow key={template.uid}>
                <TableCell>{template.name}</TableCell>
                <TableCell>{template.containers}</TableCell>
                <TableCell>
                  {template.create_time
                    ? formatDateValue(template.create_time, i18n.language)
                    : DASH}
                </TableCell>
                <TableCell>
                  <Button variant="danger" size="sm" onClick={() => remove(template)}>
                    {t('projects.delete')}
                  </Button>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      ) : (
        <EmptyState
          title={t('templates.empty')}
          description={t('templates.empty_description')}
        />
      )}
    </div>
  );
}
