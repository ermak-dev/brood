// API не отдаёт список участников и не раскрывает незавершённую retry-операцию.
export const accessCapabilities = { listMembers: false, retrySwarm: false } as const;

const memberPaths = new Set(['/projects', '/swarms', '/permissions', '/actors', '/templates']);

export function visibleAccessPath(path: string, isSuperuser: boolean): boolean {
  return isSuperuser || memberPaths.has(path);
}
