/** Адреса раздела Workflows. Прежние /pipelines и /pipelines/:id ведут сюда
 * с тем же ID прогона и строкой запроса: сохранённые ссылки не ломаются. */
export const WORKFLOWS_PATH = '/workflows';

export function workflowRunPath(id: string): string {
  return `${WORKFLOWS_PATH}/runs/${encodeURIComponent(id)}`;
}

export function legacyPipelinePath(id: string | undefined, search: string): string {
  return `${id ? workflowRunPath(id) : WORKFLOWS_PATH}${search}`;
}
