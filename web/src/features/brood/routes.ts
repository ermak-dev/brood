export const projectPath = (id: string) => `/projects/${encodeURIComponent(id)}`;
export const swarmPath = (id: string) => `/swarms/${encodeURIComponent(id)}`;

/** Sections open to every signed-in user; everything else is for an
 * administrator. The server checks each request again. */
const memberPaths = ['/profile', '/projects', '/swarms'];

export function visiblePath(path: string, administrator: boolean): boolean {
  if (administrator || path === '/') return true;
  return memberPaths.some((allowed) => path === allowed || path.startsWith(`${allowed}/`));
}
