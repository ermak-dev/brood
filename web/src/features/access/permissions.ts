// Обычному пользователю видны только ресурсные разделы. Сервер
// повторно проверяет членство и владение при каждом запросе.
const memberPaths = ['/projects', '/swarms', '/actors', '/templates', '/pipelines'];

export function visibleAccessPath(path: string, isSuperuser: boolean): boolean {
  if (isSuperuser) return true;
  if (path.startsWith('/projects/') && path.endsWith('/members')) return false;
  return memberPaths.some((allowed) => path === allowed || path.startsWith(`${allowed}/`));
}
