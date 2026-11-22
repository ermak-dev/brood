/** Sections open to every signed-in user; everything else is for an
 * administrator. The server checks each request again. */
const memberPaths = ['/profile', '/projects', '/roles', '/swarms', '/agents'];

/** Where a signed-in user lands: agents are what people come for. */
export const HOME_PATH = '/agents';

/** The last query of each table in this tab, so a parent link returns to a
 * table as it was left. Memory of the page, not of the browser: a reload
 * starts clean and the address stays the only durable state. */
const lastQuery = new Map<string, string>();

/** What a table keeps when it is entered through a parent link: its sort
 * and its state filter. The text filter and the open panel belonged to the
 * rows that were on screen then. */
const KEPT = ['sort', 'dir', 'state'];

export function rememberQuery(path: string, query: string) {
  lastQuery.set(path, query);
}

function tableLink(path: string, filters: Record<string, string>) {
  const before = new URLSearchParams(lastQuery.get(path) ?? '');
  const next = new URLSearchParams();
  for (const [name, value] of Object.entries(filters)) if (value) next.set(name, value);
  for (const name of KEPT) {
    const value = before.get(name);
    if (value) next.set(name, value);
  }
  const query = next.toString();
  return query ? `${path}?${query}` : path;
}

/** The swarm table narrowed to a project. */
export const swarmsOf = (project: string) => tableLink('/swarms', { project });
/** The agent table narrowed to a swarm (and to its project, when known, so
 * both filters of the table agree). */
export const agentsOf = (swarm: string, project = '') => tableLink('/agents', { project, swarm });
/** A table with one of its objects open in the side panel. */
export const panelOf = (path: string, id: string) => `${path}?${new URLSearchParams({ open: id })}`;
/** A table with its create sheet open and the parent preselected. */
export const createIn = (path: string, parent: Record<string, string>) =>
  `${path}?${new URLSearchParams({ ...parent, create: '1' })}`;

export function visiblePath(path: string, administrator: boolean): boolean {
  if (administrator || path === '/') return true;
  return memberPaths.some((allowed) => path === allowed || path.startsWith(`${allowed}/`));
}
