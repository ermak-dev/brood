import { useCallback, useId, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { EmptyState } from '@/components/ui/EmptyState';
import { pipelinesApi, type WorkflowDefinition } from '@/services/brood/pipelines';
import { LoadState } from '@/features/access/shared';
import { useObjectSelection } from '@/features/objects/useObjectSelection';
import { CopyButton } from '@/features/oauth/components/CopyButton';
import { SelectionNotice } from '@/features/substrate/components/SelectionNotice';
import { useResource } from '@/features/substrate/hooks';
import { shortCommit } from '@/features/swarms/swarmState';
import { formatDateTimeValue } from '@/utils/format';
import { catalogSelection, definitionKey } from './catalog';
import { RunGraph } from './RunGraph';
import styles from '@/features/substrate/substrate.module.scss';
import local from './WorkflowCatalog.module.scss';

/** Каталог определений проекта на его странице (#240): читает любой, кто
 * видит проект, — тот же `GET /api/projects/{id}/workflow-catalog`, что у
 * диалога первого запуска (#218). Список — радиогруппой: стрелки выбирают,
 * выбор сразу показывает граф; без выбора показан первый. Показанное
 * определение живёт в адресе (`?workflow=`, #270): выбор пишет его имя на
 * место текущей записи истории, «Назад» и «Вперёд» показывают определение
 * своего адреса, имя, которого в каталоге нет, названо с действием, снимающим
 * выбор (#194). Граф -- тот же компонент, что граф роя, только для чтения, и
 * ровно той версии и ревизии, что в списке (#199). Это не снимок роя: новая
 * публикация меняет каталог, а не рой, начатый на прежней. */
export function WorkflowCatalog({ projectId }: { projectId: string }) {
  const { t } = useTranslation();
  const titleId = useId();
  const load = useCallback(() => pipelinesApi.listDefinitions(projectId), [projectId]);
  const catalog = useResource(load);
  const { selection, replace } = useObjectSelection();
  // Повтор графа перечитывает каталог (409 значит: он ушёл вперёд) и заново
  // открывает граф — той публикации, что теперь в списке.
  const [attempt, setAttempt] = useState(0);
  const retryGraph = async () => {
    await catalog.refresh();
    setAttempt((value) => value + 1);
  };
  const definitions = catalog.data;
  const { shown, missing } = catalogSelection(definitions, selection.workflow);
  const choose = (name: string | null) => replace({ workflow: name });
  return (
    <section
      className={`${styles.page} ${local.catalog}`}
      aria-labelledby={titleId}
      data-catalog=""
    >
      <h2 id={titleId}>{t('pipelines.catalog.title')}</h2>
      <p className={styles.sectionHint}>{t('pipelines.catalog.hint')}</p>
      <LoadState loading={catalog.loading} error={catalog.error ?? ''} retry={catalog.refresh}>
        {definitions && definitions.length > 0 ? (
          <>
            {missing && selection.workflow && (
              <SelectionNotice
                text={t('pipelines.catalog.missing', { name: selection.workflow })}
                action={t('pipelines.catalog.clear_selection')}
                onAction={() => choose(null)}
              />
            )}
            <div className={local.layout}>
              <DefinitionList definitions={definitions} shown={shown} choose={choose} />
              {shown && (
                <DefinitionGraph
                  key={`${definitionKey(shown)}:${attempt}`}
                  projectId={projectId}
                  definition={shown}
                  retry={retryGraph}
                />
              )}
            </div>
          </>
        ) : (
          <div className={`${styles.empty} ${local.empty}`} data-catalog-empty="">
            <EmptyState
              title={t('pipelines.catalog.empty')}
              description={t('pipelines.catalog.empty_hint')}
            />
          </div>
        )}
      </LoadState>
    </section>
  );
}

/** Строка определения: имя — подпись радиокнопки, версия, короткая ревизия
 * (полная — в подсказке) и момент публикации — её описание. Подпись
 * растянута на всю строку: выбирает щелчок в любом её месте. Без показанного
 * (имя адреса не в каталоге) ни одна строка не отмечена. */
function DefinitionList({
  definitions,
  shown,
  choose,
}: {
  definitions: WorkflowDefinition[];
  shown: WorkflowDefinition | undefined;
  choose: (name: string) => void;
}) {
  const { t, i18n } = useTranslation();
  const group = useId();
  return (
    <div role="radiogroup" aria-label={t('pipelines.catalog.choose')} className={local.list}>
      {definitions.map((definition, index) => {
        const id = `${group}-${index}`;
        const time = formatDateTimeValue(definition.publishedAt, i18n.language);
        return (
          <div key={definition.name} className={local.row} data-definition={definition.name}>
            <input
              type="radio"
              id={id}
              name={group}
              value={definition.name}
              checked={definition.name === shown?.name}
              onChange={() => choose(definition.name)}
              aria-describedby={`${id}-version ${id}-meta`}
            />
            <label htmlFor={id} className={local.name}>
              {definition.name}
            </label>
            <span id={`${id}-version`} className={local.version}>
              {t('pipelines.catalog.version', { version: definition.version })}
            </span>
            <span id={`${id}-meta`} className={local.meta}>
              <code
                className={local.revision}
                title={t('pipelines.catalog.revision', { revision: definition.revision })}
              >
                {shortCommit(definition.revision)}
              </code>
              <span>
                {time
                  ? t('pipelines.catalog.published', { time })
                  : t('pipelines.catalog.published_unknown')}
              </span>
            </span>
          </div>
        );
      })}
    </div>
  );
}

/** Граф показанного определения в пунктирной рамке, как в диалоге запуска:
 * это чертёж, а не исполнение. Отказ сервера — его причиной, повтор
 * перечитывает каталог. */
function DefinitionGraph({
  projectId,
  definition,
  retry,
}: {
  projectId: string;
  definition: WorkflowDefinition;
  retry: () => Promise<void>;
}) {
  const { t } = useTranslation();
  const headingId = useId();
  const { name, version, revision } = definition;
  const load = useCallback(
    () => pipelinesApi.getDefinitionGraph(projectId, name, version, revision),
    [projectId, name, version, revision]
  );
  // Граф публикации неизменен: перечитывание шапки трогает только каталог.
  const graph = useResource(load, { headerRefresh: false });
  const label = t('pipelines.catalog.graph', { name, version });
  return (
    <section className={local.graph} aria-labelledby={headingId} data-catalog-graph="">
      <header className={local.graphHead}>
        <h3 id={headingId} className={local.graphName}>
          {name}
        </h3>
        <p className={local.graphFacts}>
          <span>{t('pipelines.catalog.version', { version })}</span>
          <code className={local.revision} title={t('pipelines.catalog.revision', { revision })}>
            {shortCommit(revision)}
          </code>
          <CopyButton value={revision} label={t('pipelines.catalog.copy_revision')} />
        </p>
      </header>
      <LoadState loading={graph.loading} error={graph.error ?? ''} retry={retry}>
        {graph.data?.svg ? (
          <RunGraph svg={graph.data.svg} label={label} />
        ) : (
          <p className={styles.fieldHint}>{t('pipelines.graph_empty')}</p>
        )}
      </LoadState>
    </section>
  );
}
