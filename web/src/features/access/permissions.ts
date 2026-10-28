const memberPaths = new Set(['/projects', '/swarms', '/permissions', '/actors', '/templates']);

export function visibleAccessPath(path: string, isSuperuser: boolean): boolean {
  return isSuperuser || memberPaths.has(path);
}
