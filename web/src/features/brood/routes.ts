const withQuery = (path: string, name: string, value: string) =>
  `${path}?${new URLSearchParams({ [name]: value })}`;

/** The swarm table narrowed to a project. */
export const swarmsOf = (project: string) => withQuery('/swarms', 'project', project);
/** The agent table narrowed to a swarm. */
export const agentsOf = (swarm: string) => withQuery('/agents', 'swarm', swarm);

/** Where a signed-in user lands: agents are what people come for. */
export const HOME_PATH = '/agents';

/** Sections open to every signed-in user; everything else is for an
 * administrator. The server checks each request again. */
const memberPaths = ['/profile', '/projects', '/swarms', '/agents'];

export function visiblePath(path: string, administrator: boolean): boolean {
  if (administrator || path === '/') return true;
  return memberPaths.some((allowed) => path === allowed || path.startsWith(`${allowed}/`));
}
