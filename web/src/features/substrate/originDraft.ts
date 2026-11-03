/** Почему черновик origin не пройдёт каноническую проверку сервера
 * (go/brood/gitorigin): только https, без userinfo/query/fragment, порт только
 * 443, имя хоста, а не IP, непустой путь, без управляющих символов, не длиннее
 * 1024 байт. Пустое значение допустимо: сервер выберет origin по умолчанию.
 * Это подсказка до отправки; окончательный режим (gateway/direct) и отказ —
 * за сервером. */
export type OriginProblem =
  | 'length'
  | 'control'
  | 'scheme'
  | 'invalid'
  | 'credentials'
  | 'query'
  | 'port'
  | 'host'
  | 'path';

const MAX_BYTES = 1024;
const IPV4 = /^\d{1,3}(\.\d{1,3}){3}$/;

export function originProblem(draft: string): OriginProblem | null {
  const value = draft.trim();
  if (!value) return null;
  if (new TextEncoder().encode(value).length > MAX_BYTES) return 'length';
  // eslint-disable-next-line no-control-regex
  if (/[\u0000-\u001f\u007f-\u009f]/.test(value)) return 'control';
  if (!/^https:\/\//i.test(value)) return 'scheme';
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    return 'invalid';
  }
  if (!url.hostname) return 'invalid';
  if (url.username || url.password) return 'credentials';
  if (value.includes('?') || value.includes('#')) return 'query';
  if (url.port && url.port !== '443') return 'port';
  if (url.hostname.startsWith('[') || IPV4.test(url.hostname)) return 'host';
  if (url.pathname === '' || url.pathname === '/') return 'path';
  return null;
}
