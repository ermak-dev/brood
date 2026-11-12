import type { WorkflowDefinition } from '@/services/brood/pipelines';

/** Определение, которое показывает каталог на странице проекта (#240):
 * названное адресом (`?workflow=`, #270) -- в его нынешней версии каталога,
 * без имени в адресе -- первое. Выбор здесь только смотрит, ничего не
 * запускает, поэтому, в отличие от диалога запуска (#218), панель сразу
 * показывает первое. Имя, которого в каталоге нет (не опубликовано, ушло
 * после перечитывания) -- missing: по правилу #194 страница называет
 * причину, а не подменяет его первым. */
export function catalogSelection(
  definitions: WorkflowDefinition[] | null,
  selected: string | undefined
): { shown: WorkflowDefinition | undefined; missing: boolean } {
  if (!definitions) return { shown: undefined, missing: false };
  if (!selected) return { shown: definitions[0], missing: false };
  const shown = definitions.find((definition) => definition.name === selected);
  return { shown, missing: shown === undefined };
}

/** Личность публикации: имя, версия и ревизия вместе (#199). Граф читается
 * ровно по ней, поэтому новая публикация того же имени — другой граф. */
export function definitionKey({ name, version, revision }: WorkflowDefinition): string {
  return `${name}:${version}:${revision}`;
}
