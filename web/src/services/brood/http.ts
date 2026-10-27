/**
 * brood: same-origin calls to the Go server with its session.
 *
 * Unsafe methods carry the csrf cookie back as X-CSRFToken; a refusal
 * becomes BroodApiError with the server's reason ({"detail": ...}) as message.
 */

export class BroodApiError extends Error {
  status: number;

  constructor(status: number, message: string) {
    super(message);
    this.name = 'BroodApiError';
    this.status = status;
  }
}

export function csrfToken(): string {
  const match = document.cookie.match(/(?:^|;\s*)csrf=([^;]+)/);
  return match ? decodeURIComponent(match[1]) : '';
}

/** A readable reason out of whatever a failed call threw. */
export function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

export async function request<T>(method: string, url: string, body?: unknown): Promise<T> {
  const unsafe = method !== 'GET';
  if (unsafe && !csrfToken()) {
    // The session endpoint sets the cookie the write must send back.
    await fetch('/api/auth/session', { credentials: 'same-origin' });
  }
  const headers: Record<string, string> = {};
  if (unsafe) headers['X-CSRFToken'] = csrfToken();
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  const response = await fetch(url, {
    method,
    credentials: 'same-origin',
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const data: unknown = response.status === 204 ? null : await response.json().catch(() => null);
  if (!response.ok) {
    if (response.status === 401 && typeof window !== 'undefined') {
      window.dispatchEvent(new Event('unauthorized'));
    }
    const failure = data as { detail?: unknown; error?: unknown } | null;
    const detail = failure?.detail ?? failure?.error;
    throw new BroodApiError(
      response.status,
      typeof detail === 'string' ? detail : `HTTP ${response.status}`
    );
  }
  return data as T;
}
