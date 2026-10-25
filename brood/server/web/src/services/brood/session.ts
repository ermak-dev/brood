/**
 * brood session: the panel signs in to the brood server (Django), not to CLIProxyAPI.
 *
 * The management key never reaches the browser. The brood edge (nginx) checks the
 * Django session on every /v8/management request and adds the key itself, so the
 * API client sends no Authorization header at all.
 */

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

const SESSION_URL = '/auth/session';
const LOGIN_URL = '/auth/login';
const LOGOUT_URL = '/auth/logout';

async function readSession(response: Response): Promise<BroodSession> {
  const body = (await response.json().catch(() => ({}))) as Record<string, unknown>;
  if (!response.ok) {
    const message = typeof body.error === 'string' ? body.error : `HTTP ${response.status}`;
    throw new BroodAuthError(response.status, message);
  }
  return {
    authenticated: body.authenticated === true,
    username: typeof body.username === 'string' ? body.username : '',
    isSuperuser: body.is_superuser === true,
  };
}

/** The current session; it also sets the csrftoken cookie that login and logout send back. */
export async function getSession(): Promise<BroodSession> {
  const response = await fetch(SESSION_URL, { credentials: 'same-origin' });
  return readSession(response);
}

async function post(url: string, payload: unknown): Promise<BroodSession> {
  if (!csrfToken()) {
    await getSession();
  }
  const response = await fetch(url, {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json', 'X-CSRFToken': csrfToken() },
    body: JSON.stringify(payload),
  });
  return readSession(response);
}

export function login(username: string, password: string): Promise<BroodSession> {
  return post(LOGIN_URL, { username, password });
}

export function logout(): Promise<BroodSession> {
  return post(LOGOUT_URL, {});
}
