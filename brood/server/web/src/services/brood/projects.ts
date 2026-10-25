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

export interface BroodActor {
  name: string;
  uid: string;
  version: number;
  state: string;
  create_time: string;
  update_time: string;
}

export interface BroodTemplate {
  name: string;
  uid: string;
  version: number;
  containers: number;
  create_time: string;
  update_time: string;
}

export interface BroodWorker {
  name: string;
  uid: string;
  version: number;
  state: string;
  worker_pool: string;
  node: string;
  actors_capacity: number;
  actors_allocated: number;
  create_time: string;
  update_time: string;
}

export interface BroodBinding {
  role: string;
  members: string[];
}

export interface BroodPolicy {
  name: string;
  uid: string;
  version: number;
  bindings: BroodBinding[];
  create_time: string;
  update_time: string;
}

export type ActorAction = 'suspend' | 'resume';

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

export async function listActors(project: string): Promise<BroodActor[]> {
  const response = await fetch(`/api/projects/${encodeURIComponent(project)}/actors`, {
    credentials: 'same-origin',
  });
  const body = await read(response);
  const results =
    body && typeof body === 'object' && Array.isArray((body as { results?: unknown }).results)
      ? (body as { results: BroodActor[] }).results
      : [];
  return results;
}

export async function actorAction(
  project: string,
  name: string,
  action: ActorAction
): Promise<BroodActor> {
  await ensureCsrf();
  const response = await fetch(
    `/api/projects/${encodeURIComponent(project)}/actors/${encodeURIComponent(name)}`,
    {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json', 'X-CSRFToken': csrfToken() },
      body: JSON.stringify({ action }),
    }
  );
  return (await read(response)) as BroodActor;
}

export async function deleteActor(project: string, name: string): Promise<void> {
  await ensureCsrf();
  const response = await fetch(
    `/api/projects/${encodeURIComponent(project)}/actors/${encodeURIComponent(name)}`,
    {
      method: 'DELETE',
      credentials: 'same-origin',
      headers: { 'X-CSRFToken': csrfToken() },
    }
  );
  await read(response);
}

export async function listTemplates(project: string): Promise<BroodTemplate[]> {
  const response = await fetch(`/api/projects/${encodeURIComponent(project)}/templates`, {
    credentials: 'same-origin',
  });
  const body = await read(response);
  const results =
    body && typeof body === 'object' && Array.isArray((body as { results?: unknown }).results)
      ? (body as { results: BroodTemplate[] }).results
      : [];
  return results;
}

export async function deleteTemplate(project: string, name: string): Promise<void> {
  await ensureCsrf();
  const response = await fetch(
    `/api/projects/${encodeURIComponent(project)}/templates/${encodeURIComponent(name)}`,
    {
      method: 'DELETE',
      credentials: 'same-origin',
      headers: { 'X-CSRFToken': csrfToken() },
    }
  );
  await read(response);
}

export async function listWorkers(): Promise<BroodWorker[]> {
  const response = await fetch('/api/workers', { credentials: 'same-origin' });
  const body = await read(response);
  const results =
    body && typeof body === 'object' && Array.isArray((body as { results?: unknown }).results)
      ? (body as { results: BroodWorker[] }).results
      : [];
  return results;
}

export async function drainWorker(name: string): Promise<BroodWorker> {
  await ensureCsrf();
  const response = await fetch(`/api/workers/${encodeURIComponent(name)}`, {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json', 'X-CSRFToken': csrfToken() },
    body: JSON.stringify({ action: 'drain' }),
  });
  return (await read(response)) as BroodWorker;
}

/** Политика глобального скоупа; project -- политика одноимённой atespace. */
export async function getPolicy(project?: string): Promise<BroodPolicy> {
  const url = project
    ? `/api/policy/projects/${encodeURIComponent(project)}`
    : '/api/policy/global';
  const response = await fetch(url, { credentials: 'same-origin' });
  return (await read(response)) as BroodPolicy;
}

/** Заменить политику целиком: шлём прочитанные uid и version (оптимистичная
 * блокировка ateapi), иначе сервер откажет. */
export async function savePolicy(
  policy: BroodPolicy,
  project?: string
): Promise<BroodPolicy> {
  await ensureCsrf();
  const url = project
    ? `/api/policy/projects/${encodeURIComponent(project)}`
    : '/api/policy/global';
  const response = await fetch(url, {
    method: 'PUT',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json', 'X-CSRFToken': csrfToken() },
    body: JSON.stringify({
      uid: policy.uid,
      version: policy.version,
      bindings: policy.bindings,
    }),
  });
  return (await read(response)) as BroodPolicy;
}

export async function deletePolicy(project: string): Promise<void> {
  await ensureCsrf();
  const response = await fetch(`/api/policy/projects/${encodeURIComponent(project)}`, {
    method: 'DELETE',
    credentials: 'same-origin',
    headers: { 'X-CSRFToken': csrfToken() },
  });
  await read(response);
}
