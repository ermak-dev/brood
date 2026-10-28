import { Fragment, type ReactNode } from 'react';
import { useNavigate, useParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { EmptyState } from '@/components/ui/EmptyState';
import { accessApi, type AccessProject } from '@/services/brood/access';
import { PageHeader } from '@/features/substrate/components/PageHeader';
import { LoadState, useAccessResource } from './shared';
import { selectAccessProject } from './state';
import styles from '@/features/substrate/substrate.module.scss';

/** brood: проект раздела берётся из маршрута, а не из второго выпадающего
 * списка рядом с глобальным выбором роя. Доступные проекты всё равно нужно
 * прочитать: по ним проверяется адрес и берётся имя для заголовка. */
export function useRouteProject() {
  const { project: requested = '' } = useParams();
  const resource = useAccessResource(accessApi.projects);
  return { ...resource, requested, project: selectAccessProject(resource.data, requested) };
}

interface ProjectRouteProps {
  title: string;
  /** Раздел, смонтированный только с доступным проектом: его хуки всегда
   * работают с одним и тем же ID. */
  children: (project: AccessProject) => ReactNode;
}

/** Разделы проекта (рои, участники) живут во вложенном маршруте. Чужой или
 * удалённый ID — не пустая таблица, а объяснение со ссылкой на список. */
export function ProjectRoute({ title, children }: ProjectRouteProps) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const selection = useRouteProject();
  if (selection.project) {
    return <Fragment key={selection.project.id}>{children(selection.project)}</Fragment>;
  }
  return (
    <div className={styles.page}>
      <PageHeader title={title} />
      <LoadState {...selection} retry={selection.refresh}>
        <div className={styles.empty}>
          <EmptyState
            title={t('access.no_projects')}
            action={<Button onClick={() => navigate('/projects')}>{t('access.projects')}</Button>}
          />
        </div>
      </LoadState>
    </div>
  );
}
