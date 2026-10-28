// Рои и участники — вложенные маршруты проекта, поэтому сравнение по
// префиксу: точное множество путей не пустило бы участника в '/projects/<id>'.
// '/swarms' и '/permissions' остались переадресацией старых ссылок внутрь
// проекта: закрыть их админу значит увести участника по старой ссылке не туда.
const memberPaths = ['/projects', '/actors', '/templates', '/swarms', '/permissions'];

export function visibleAccessPath(path: string, isSuperuser: boolean): boolean {
  return (
    isSuperuser || memberPaths.some((allowed) => path === allowed || path.startsWith(`${allowed}/`))
  );
}
