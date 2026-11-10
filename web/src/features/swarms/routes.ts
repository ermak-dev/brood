/** Адреса роя (#215): список роёв проекта и страница одного роя. ID в пути
 * экранируется: рой открывается ровно тем ID, который назван ссылкой. */
export const SWARMS_PATH = '/swarms';

export function swarmPath(id: string): string {
  return `${SWARMS_PATH}/${encodeURIComponent(id)}`;
}

/** Старые адреса прогона (/workflows/runs/:id, /pipelines/:id) и раздела
 * Workflows (#217). ID прогона — ID роя (#206), поэтому прогон открывает
 * страницу своего роя, а раздел — список роёв. Строка запроса едет дальше:
 * ?project= по-прежнему выбирает проект. */
export function legacyRunPath(id: string | undefined, search: string): string {
  return `${id ? swarmPath(id) : SWARMS_PATH}${search}`;
}
