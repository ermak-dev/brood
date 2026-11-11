import type { WorkflowDefinition } from '@/services/brood/pipelines';

/** Определение, которое показывает каталог на странице проекта (#240):
 * выбранное по имени — в его нынешней версии каталога, иначе первое. Выбор
 * здесь только смотрит, ничего не запускает, поэтому, в отличие от диалога
 * запуска (#218), панель сразу показывает первое; выбор, ушедший из каталога
 * после перечитывания, тоже уступает первому. */
export function catalogSelection(
  definitions: WorkflowDefinition[] | null,
  chosen: string
): WorkflowDefinition | undefined {
  if (!definitions) return undefined;
  return definitions.find((definition) => definition.name === chosen) ?? definitions[0];
}

/** Личность публикации: имя, версия и ревизия вместе (#199). Граф читается
 * ровно по ней, поэтому новая публикация того же имени — другой граф. */
export function definitionKey({ name, version, revision }: WorkflowDefinition): string {
  return `${name}:${version}:${revision}`;
}
