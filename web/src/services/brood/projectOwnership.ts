import { request } from './http';

export interface ProjectOwnership {
  projectId: string;
  viewerUid: string;
  revision: number;
  ownerUid: string;
  ownerUsername: string;
  updatedBy: string;
  updatedAt: string;
}

/** Revision zero — отсутствие назначения, не владелец по умолчанию. */
export function parseProjectOwnership(value: unknown): ProjectOwnership {
  const invalid = () => new Error('Invalid project ownership response');
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw invalid();
  const row = value as Record<string, unknown>;
  if (
    typeof row.projectId !== 'string' ||
    !row.projectId ||
    row.projectId.length > 256 ||
    typeof row.viewerUid !== 'string' ||
    !row.viewerUid ||
    row.viewerUid.length > 256 ||
    !Number.isSafeInteger(row.revision) ||
    Number(row.revision) < 0 ||
    !['ownerUid', 'ownerUsername', 'updatedBy', 'updatedAt'].every(
      (key) => typeof row[key] === 'string' && (row[key] as string).length <= 256
    )
  )
    throw invalid();
  if (row.revision === 0) {
    if (row.ownerUid || row.ownerUsername || row.updatedBy || row.updatedAt) throw invalid();
  } else if (
    !row.updatedBy ||
    !row.updatedAt ||
    Boolean(row.ownerUid) !== Boolean(row.ownerUsername)
  )
    throw invalid();
  return value as ProjectOwnership;
}

/** Это только видимость редактора; актуальное право снова проверяет Go API. */
export function canEditProjectPolicy(
  administrator: boolean,
  userId: string,
  ownership: ProjectOwnership | undefined,
  failed = false
): boolean {
  if (administrator) return true;
  return (
    !failed &&
    Boolean(userId) &&
    Boolean(ownership && ownership.revision > 0 && ownership.ownerUid === userId)
  );
}

function ownerPath(projectId: string): string {
  if (!projectId || projectId.length > 256) throw new Error('A bounded project ID is required');
  return `/api/projects/${encodeURIComponent(projectId)}/owner`;
}

function scopedOwner(value: unknown, projectId: string): ProjectOwnership {
  const owner = parseProjectOwnership(value);
  if (owner.projectId !== projectId) throw new Error('Project ownership response scope changed');
  return owner;
}

export const projectOwnershipApi = {
  get: async (projectId: string): Promise<ProjectOwnership> =>
    scopedOwner(await request<unknown>('GET', ownerPath(projectId)), projectId),
  set: async (projectId: string, revision: number, username: string): Promise<ProjectOwnership> => {
    if (
      !Number.isSafeInteger(revision) ||
      revision < 0 ||
      revision >= Number.MAX_SAFE_INTEGER ||
      username.length > 256
    ) {
      throw new Error('Invalid project ownership selection');
    }
    const owner = scopedOwner(
      await request<unknown>('PUT', ownerPath(projectId), { revision, username }),
      projectId
    );
    if (owner.revision !== revision + 1 || owner.ownerUsername !== username) {
      throw new Error('Project ownership response does not retain the selected assignment');
    }
    return owner;
  },
};
