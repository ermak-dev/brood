/** Адреса роя (#215): список роёв проекта и страница одного роя. ID в пути
 * экранируется: рой открывается ровно тем ID, который назван ссылкой. */
export const SWARMS_PATH = '/swarms';

export function swarmPath(id: string): string {
  return `${SWARMS_PATH}/${encodeURIComponent(id)}`;
}
