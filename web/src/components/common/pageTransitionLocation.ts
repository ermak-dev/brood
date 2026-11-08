import type { Location } from 'react-router-dom';
import type { LayerStatus } from './PageTransitionLayer';

export type PageLayer = {
  key: string;
  location: Location;
  status: LayerStatus;
};

/** Что делать слоям при новом location роутера: ничего, обновить текущий
 * слой на месте или сменить страницу с анимацией. */
export type LocationChange = 'none' | 'update' | 'navigate';

// Hash history даёт ключ "default" каждой записи без history state: адресной
// строке, location.hash, навигации DevTools. Такие ключи не различают записи.
const DEFAULT_LOCATION_KEY = 'default';

/** Страница меняется только со сменой pathname; смена query, hash или записи
 * истории на той же странице доходит до неё без перемонтирования. Ключи не
 * участвуют в выборе страницы: у двух прямых переходов они совпадают. */
export function classifyLocationChange(
  current: Location | undefined,
  next: Location
): LocationChange {
  if (!current || current.pathname !== next.pathname) return 'navigate';
  if (current.key === next.key && current.search === next.search && current.hash === next.hash && current.state === next.state) {
    return 'none';
  }
  return 'update';
}

/** Записи без ключа нельзя надёжно различить, даже когда адрес совпадает. */
export function historyEntryId(location: Location): string | null {
  return location.key === DEFAULT_LOCATION_KEY ? null : location.key;
}

/** Слой той же записи истории, что location, или -1. */
export function findLayerIndex(layers: PageLayer[], location: Location): number {
  const entry = historyEntryId(location);
  return entry === null ? -1 : layers.findIndex((layer) => historyEntryId(layer.location) === entry);
}

/** React key нового слоя: ключ location, если он свободен, иначе он же с
 * первым свободным суффиксом. Повтор ключа склеил бы два слоя в один. */
export function createLayerKey(locationKey: string, takenKeys: Iterable<string>): string {
  const taken = new Set(takenKeys);
  if (!taken.has(locationKey)) return locationKey;
  let suffix = 1;
  while (taken.has(`${locationKey}~${suffix}`)) suffix += 1;
  return `${locationKey}~${suffix}`;
}

/** Текущий слой получает новый location и сохраняет React key: страница
 * не перемонтируется и видит новые query-параметры. */
export function updateCurrentLayerLocation(layers: PageLayer[], location: Location): PageLayer[] {
  const currentIndex = layers.findIndex((layer) => layer.status === 'current');
  const targetIndex = currentIndex === -1 ? layers.length - 1 : currentIndex;
  return layers.map((layer, index) => (index === targetIndex ? { ...layer, location } : layer));
}
