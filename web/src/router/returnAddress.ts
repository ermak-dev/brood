// Адрес возврата после входа (#262). ProtectedRoute уводит на вход с
// state.from — location роутера; вернуться надо на весь адрес: путь, query
// и hash, а не только на путь. State записи истории приходит из браузера,
// поэтому принимается только внутренний путь роутера: «//host» и «/\host»
// браузер читает как чужой origin, а табуляцию и перевод строки из URL
// выбрасывает, и «/\t/host» становится «//host».
const PATH = /^\/(?![/\\])[^?#\p{Cc}]*$/u;
const SEARCH = /^(\?[^#\p{Cc}]*)?$/u;
const HASH = /^(#[^\p{Cc}]*)?$/u;

function part(value: unknown, key: string): unknown {
  return value && typeof value === 'object' ? (value as Record<string, unknown>)[key] : undefined;
}

/** Внутренний адрес роутера из state.from, иначе корень. */
export function returnAddress(state: unknown): string {
  const from = part(state, 'from');
  const pathname = part(from, 'pathname');
  const search = part(from, 'search') ?? '';
  const hash = part(from, 'hash') ?? '';
  if (typeof pathname !== 'string' || typeof search !== 'string' || typeof hash !== 'string') {
    return '/';
  }
  if (!PATH.test(pathname) || !SEARCH.test(search) || !HASH.test(hash)) return '/';
  return `${pathname}${search}${hash}`;
}
