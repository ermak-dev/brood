import { useSearchParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { accessApi } from '@/services/brood/access';
import { Field } from '@/features/substrate/components/form';
import { LoadState, useAccessResource } from './shared';
import { selectAccessProject } from './state';

// Выбор проекта живёт только в адресе страницы, не в Substrate swarm store.
export function useAccessProject() {
  const [params, setParams] = useSearchParams();
  const resource = useAccessResource(accessApi.projects);
  const requested = params.get('project');
  const project = selectAccessProject(resource.data, requested);
  const select = (id: string) => setParams({ project: id });
  return { ...resource, project, select };
}

export function ProjectSelect({ selection }: { selection: ReturnType<typeof useAccessProject> }) {
  const { t } = useTranslation();
  return (
    <LoadState {...selection} retry={selection.refresh}>
      <Field label={t('access.project')}>
        {(id) => (
          <select
            id={id}
            className="input"
            value={selection.project?.id ?? ''}
            onChange={(event) => selection.select(event.target.value)}
          >
            {!selection.project && <option value="">{t('access.select_project')}</option>}
            {selection.data?.map((project) => (
              <option key={project.id} value={project.id}>
                {project.name}
              </option>
            ))}
          </select>
        )}
      </Field>
      {selection.data?.length === 0 && <p>{t('access.no_projects')}</p>}
    </LoadState>
  );
}
