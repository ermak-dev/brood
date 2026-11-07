import type { WorkflowDefinition } from '@/services/brood/pipelines';

export function selectedGraphKey(
  projectId: string,
  selected: string,
  definitions: WorkflowDefinition[] | null,
  loading: boolean,
  error: string
): string | null {
  if (loading || error) return null;
  const definition = definitions?.find((item) => item.name === selected);
  return definition
    ? JSON.stringify([projectId, definition.name, definition.version, definition.revision])
    : null;
}
