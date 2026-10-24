/**
 * brood projects: atespaces of the install's Substrate, via the brood server
 * (Django /api/projects), not via CLIProxyAPI. Same-origin fetch with the
 * Django session; writes carry the csrftoken cookie back, like the session
 * service.
 */

export interface BroodProject {
  name: string;
  uid: string;
  version: number;
  create_time: string;
  update_time: string;
}

export class ProjectsApiError extends Error {
  status: number;

  constructor(status: number, message: string) {
    super(message);
    this.name = 'ProjectsApiError';
    this.status = status;
  }
}

async function read(response: Response): Promise<unknown> {
  const body: unknown = await response.json().catch(() => null);
  if (!response.ok) {
    const detail =
      body && typeof body === 'object' && typeof (body as { detail?: unknown }).detail === 'string'
        ? (body as { detail: string }).detail
        : `HTTP ${response.status}`;
    throw new ProjectsApiError(response.status, detail);
  }
  return body;
}

function csrfToken(): string {
  const match = document.cookie.match(/(?:^|;\s*)csrftoken=([^;]+)/);
  return match ? decodeURIComponent(match[1]) : '';
}

async function ensureCsrf(): Promise<void> {
  if (!csrfToken()) {
    await fetch('/auth/session', { credentials: 'same-origin' });
  }
}

export async function listProjects(): Promise<BroodProject[]> {
  const response = await fetch('/api/projects', { credentials: 'same-origin' });
  const body = await read(response);
  const results =
    body && typeof body === 'object' && Array.isArray((body as { results?: unknown }).results)
      ? (body as { results: BroodProject[] }).results
      : [];
  return results;
}

export async function createProject(name: string): Promise<BroodProject> {
  await ensureCsrf();
  const response = await fetch('/api/projects', {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json', 'X-CSRFToken': csrfToken() },
    body: JSON.stringify({ name }),
  });
  return (await read(response)) as BroodProject;
}

export async function deleteProject(name: string): Promise<void> {
  await ensureCsrf();
  const response = await fetch(`/api/projects/${encodeURIComponent(name)}`, {
    method: 'DELETE',
    credentials: 'same-origin',
    headers: { 'X-CSRFToken': csrfToken() },
  });
  await read(response);
}
