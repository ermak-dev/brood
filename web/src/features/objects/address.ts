import { swarmPath } from '@/features/swarms/routes';

/** Адрес выбранного объекта (#194): проект, рой и объект -- параметры query
 * страницы своего типа. Порядок -- порядок адреса: так ссылка читается
 * слева направо от проекта к объекту. */
export const SELECTION_PARAMS = [
  'project',
  'swarm',
  'actor',
  'template',
  'worker',
  'pool',
  'proxmox',
] as const;

export type SelectionParam = (typeof SELECTION_PARAMS)[number];
export type Selection = Partial<Record<SelectionParam, string>>;
/** Значение задаёт параметр; null и пустая строка его снимают. */
export type SelectionChange = Partial<Record<SelectionParam, string | null>>;

/** Объект, на который ведёт ссылка. Прогон -- это рой (#206): ссылка на
 * прогон ведёт на страницу роя. Актор и шаблон без имени -- список роя. */
export type ObjectRef =
  | { kind: 'swarm'; id: string }
  | { kind: 'run'; swarm: string }
  | { kind: 'actor'; project: string; swarm: string; name?: string }
  | { kind: 'template'; project: string; swarm: string; name?: string }
  | { kind: 'worker'; name: string }
  | { kind: 'pool'; name: string }
  | { kind: 'proxmoxPool'; name: string };

/** Параметры адреса, которые выбирают объект; прочие параметры не выбор. */
export function readSelection(search: string): Selection {
  const params = new URLSearchParams(search);
  const selection: Selection = {};
  for (const key of SELECTION_PARAMS) {
    const value = params.get(key);
    if (value) selection[key] = value;
  }
  return selection;
}

/** Строка запроса с изменённым выбором. Прежний параметр меняется на своём
 * месте (повторы снимаются), новый встаёт в порядке адреса, чужие
 * параметры остаются как были. */
export function withSelection(search: string, changes: SelectionChange): string {
  const params = new URLSearchParams(search);
  for (const key of SELECTION_PARAMS) {
    if (!(key in changes)) continue;
    const value = changes[key];
    if (value) params.set(key, value);
    else params.delete(key);
  }
  const query = params.toString();
  return query ? `?${query}` : '';
}

function pathWith(pathname: string, selection: SelectionChange): string {
  return `${pathname}${withSelection('', selection)}`;
}

/** Адрес объекта: страница его типа и выбор на ней. */
export function objectPath(ref: ObjectRef): string {
  switch (ref.kind) {
    case 'swarm':
      return swarmPath(ref.id);
    case 'run':
      return swarmPath(ref.swarm);
    case 'actor':
      return pathWith('/actors', { project: ref.project, swarm: ref.swarm, actor: ref.name });
    case 'template':
      return pathWith('/templates', { project: ref.project, swarm: ref.swarm, template: ref.name });
    case 'worker':
      return pathWith('/workers', { worker: ref.name });
    case 'pool':
      return pathWith('/worker-pools', { pool: ref.name });
    case 'proxmoxPool':
      return pathWith('/worker-pools', { proxmox: ref.name });
  }
}

/** Актор или шаблон, выбранный в группе роя. Имена повторяются в разных
 * роях, поэтому без роя в адресе имя панели не открывает. */
export function selectedIn(
  selection: Selection,
  swarm: string,
  param: 'actor' | 'template'
): string | undefined {
  return selection.swarm === swarm ? selection[param] : undefined;
}

/** Рои, которые показывает страница: рой адреса или все. Рой адреса,
 * которого нет среди доступных, -- missing: страница говорит об этом, а не
 * подменяет его всеми роями молча. */
export function swarmsShown<T extends { id: string }>(
  swarms: T[],
  selected: string | undefined
): { shown: T[]; missing: boolean } {
  if (!selected) return { shown: swarms, missing: false };
  const shown = swarms.filter((swarm) => swarm.id === selected);
  return { shown, missing: shown.length === 0 };
}

/** State записи истории, которую открыла строка списка. */
export const PANEL_ENTRY = { panel: 'list' } as const;

function openedFromList(state: unknown): boolean {
  return (
    typeof state === 'object' &&
    state !== null &&
    (state as { panel?: unknown }).panel === PANEL_ENTRY.panel
  );
}

/** Куда ведёт закрытие панели. Открытая строкой списка -- шаг назад к этому
 * списку: история не копит пар «открыл-закрыл», а фильтр, которого не было
 * до открытия, не остаётся. Открытая ссылкой -- тот же адрес без параметра
 * объекта. */
export function closeTarget(
  state: unknown,
  search: string,
  param: SelectionParam
): { back: true } | { search: string } {
  if (openedFromList(state)) return { back: true };
  return { search: withSelection(search, { [param]: null }) };
}

/** Показ панели объекта; session меняет key, чтобы новый показ начинался с
 * объекта как прочитан. */
export interface PanelState {
  name: string;
  open: boolean;
  session: number;
}

export const CLOSED_PANEL: PanelState = { name: '', open: false, session: 0 };

/** Панель следует за адресом: новый объект или повторное открытие -- новый
 * показ; закрытие оставляет имя, пока панель уезжает. */
export function followPanel(panel: PanelState, selected: string | undefined): PanelState {
  if (!selected) return panel.open ? { ...panel, open: false } : panel;
  if (panel.open && panel.name === selected) return panel;
  return { name: selected, open: true, session: panel.session + 1 };
}
