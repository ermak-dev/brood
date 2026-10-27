import { csrfToken } from './http';

export interface BroodSession {
  authenticated: boolean;
  username: string;
  isSuperuser: boolean;
}

export class BroodAuthError extends Error {
  status: number;

  constructor(status: number, message: string) {
    super(message);
    this.name = 'BroodAuthError';
    this.status = status;
  }
}

export async function getSession(): Promise<BroodSession> {
  const response = await fetch('/api/auth/session', { credentials: 'same-origin' });
  const body = (await response.json().catch(() => ({}))) as Record<string, unknown>;
  if (!response.ok) {
    throw new BroodAuthError(
      response.status,
      typeof body.detail === 'string'
        ? body.detail
        : typeof body.error === 'string'
          ? body.error
          : `HTTP ${response.status}`
    );
  }
  const user = body.user as Record<string, unknown> | undefined;
  return {
    authenticated: body.authenticated === true,
    username: typeof user?.username === 'string' ? user.username : '',
    isSuperuser: user?.is_admin === true,
  };
}

async function post(url: string, payload: unknown): Promise<void> {
  const response = await fetch(url, {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json', 'X-CSRFToken': csrfToken() },
    body: JSON.stringify(payload),
  });
  if (!response.ok) {
    const body = (await response.json().catch(() => ({}))) as Record<string, unknown>;
    throw new BroodAuthError(
      response.status,
      typeof body.detail === 'string'
        ? body.detail
        : typeof body.error === 'string'
          ? body.error
          : `HTTP ${response.status}`
    );
  }
}

export async function login(username: string, password: string): Promise<BroodSession> {
  await getSession();
  await post('/api/auth/login', { username, password });
  return getSession();
}

export async function logout(): Promise<void> {
  if (!csrfToken()) await getSession();
  await post('/api/auth/logout', {});
}
