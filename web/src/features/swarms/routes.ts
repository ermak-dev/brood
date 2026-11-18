/** Адреса роя (#215): список роёв проекта и страница одного роя. ID в пути
 * экранируется: рой открывается ровно тем ID, который назван ссылкой. */
export const SWARMS_PATH = '/swarms';

export function swarmPath(id: string): string {
  return `${SWARMS_PATH}/${encodeURIComponent(id)}`;
}

export function swarmPassPath(id: string, pass: number): string {
  if (!id || !Number.isSafeInteger(pass) || pass < 1)
    throw new Error('Swarm and positive pass number are required');
  return `${swarmPath(id)}/passes/${pass}`;
}

export function swarmPassComparisonPath(id: string, pass: number): string {
  return `${swarmPassPath(id, pass)}/compare`;
}

/** Старые адреса прогона (/workflows/runs/:id, /pipelines/:id) и раздела
 * Workflows (#217). ID прогона — ID роя (#206), поэтому прогон открывает
 * страницу своего роя, а раздел — список роёв. Строка запроса едет дальше:
 * ?project= по-прежнему выбирает проект. */
export function legacyRunPath(id: string | undefined, search: string): string {
  return `${id ? swarmPath(id) : SWARMS_PATH}${search}`;
}
