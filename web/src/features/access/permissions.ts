// Обычному пользователю видны выбранный проект, его ресурсные разделы и
// каталог проектов. /pipelines открыт, чтобы старые ссылки участника дошли
// до переадресации на /workflows. Сервер повторно проверяет членство и
// владение при каждом запросе.
const memberPaths = [
  '/project',
  '/projects',
  '/swarms',
  '/actors',
  '/templates',
  '/workflows',
  '/pipelines',
];

export function visibleAccessPath(path: string, isSuperuser: boolean): boolean {
  if (isSuperuser) return true;
  if (path.startsWith('/projects/') && path.endsWith('/members')) return false;
  return memberPaths.some((allowed) => path === allowed || path.startsWith(`${allowed}/`));
}
