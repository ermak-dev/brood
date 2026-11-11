// Обычному пользователю видны обзор выбранного проекта (стартовая страница,
// #197), проект, его ресурсные разделы и каталог проектов. /workflows и
// /pipelines открыты, чтобы старые ссылки участника на прогон дошли до
// переадресации на страницу роя (#217). Сервер повторно проверяет членство
// и владение при каждом запросе.
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
  // Корень -- только сам: под ним живут все разделы.
  if (path === '/') return true;
  if (path.startsWith('/projects/') && path.endsWith('/members')) return false;
  return memberPaths.some((allowed) => path === allowed || path.startsWith(`${allowed}/`));
}
