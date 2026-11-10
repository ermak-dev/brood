import {
  useCallback,
  useEffect,
  useId,
  useRef,
  useState,
  type FormEvent,
  type ReactNode,
  type RefObject,
} from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { Modal } from '@/components/ui/Modal';
import { useNotificationStore } from '@/stores';
import type { AccessSwarm } from '@/services/brood/access';
import { executionApi } from '@/services/brood/execution';
import { errorMessage } from '@/services/brood/http';
import {
  pipelinesApi,
  type PipelineRun,
  type SnapshotIdentity,
  type WorkflowDefinition,
} from '@/services/brood/pipelines';
import { LoadState } from '@/features/access/shared';
import { RunGraph } from '@/features/pipelines/RunGraph';
import { useResource } from '@/features/substrate/hooks';
import { formatDateTimeValue } from '@/utils/format';
import {
  definitionChoice,
  goalBytes,
  goalProblem,
  MAX_GOAL_BYTES,
  restartDefinition,
  snapshotIdentity,
  type ExecutionAction,
  type GoalProblem,
} from './swarmControl';
import { passGoal } from './swarmState';
import { StartRolesNote } from './SwarmRoles';
import styles from '@/features/substrate/substrate.module.scss';
import page from './SwarmPage.module.scss';
import local from './SwarmControls.module.scss';

/** Личность снимка роя (#218): определение, версия и ревизия, которые
 * записал первый запуск. Это не текущее определение каталога: расхождение
 * с каталогом видно по версии и ревизии, а не угадывается по имени. */
export function SnapshotFacts({ identity }: { identity: SnapshotIdentity }) {
  const { t } = useTranslation();
  const labelId = useId();
  return (
    <section className={local.snapshot} aria-labelledby={labelId}>
      <p id={labelId} className={local.snapshotLabel}>
        {t('swarm_page.snapshot.label')}
      </p>
      <dl className={local.snapshotFacts}>
        <div>
          <dt>{t('swarm_page.snapshot.definition')}</dt>
          <dd>{identity.name}</dd>
        </div>
        <div>
          <dt>{t('swarm_page.snapshot.version')}</dt>
          <dd>{identity.version}</dd>
        </div>
        <div>
          <dt>{t('swarm_page.snapshot.revision')}</dt>
          <dd>
            <code className={local.revision}>{identity.revision}</code>
          </dd>
        </div>
      </dl>
      <p className={local.snapshotHint}>{t('swarm_page.snapshot.hint')}</p>
    </section>
  );
}

type Dialog = { action: ExecutionAction; run: PipelineRun | null; session: number; open: boolean };

/** Действие исполнения в шапке страницы роя (#218) и его диалог. Кнопка
 * одна и та же: после запуска она становится остановкой, и фокус, который
 * диалог возвращает ей, не теряется. Открытый диалог помнит действие, с
 * которым его открыли: перечитанное состояние не подменяет его посреди
 * подтверждения, а устаревшее действие сервер отвергнет с причиной. Каждое
 * открытие — новый диалог (session): выбор и отказ прошлого не переживают.
 * Выбранное определение первого запуска диалог сообщает странице (chose):
 * блок ролей показывает роли именно его (#241). */
export function SwarmExecutionControl({
  swarm,
  projectName,
  run,
  action,
  changed,
  chose,
}: {
  swarm: AccessSwarm;
  projectName: string;
  run: PipelineRun | null;
  action: ExecutionAction | null;
  changed: () => Promise<void>;
  chose?: (definition: string) => void;
}) {
  const { t } = useTranslation();
  const [dialog, setDialog] = useState<Dialog | null>(null);
  const open = () => {
    if (!action) return;
    setDialog((last) => ({ action, run, session: (last?.session ?? 0) + 1, open: true }));
  };
  const close = useCallback(() => setDialog((last) => last && { ...last, open: false }), []);
  return (
    <>
      {action && (
        <Button
          variant={action === 'stop' ? 'secondary' : 'primary'}
          aria-haspopup="dialog"
          onClick={open}
        >
          {t(`swarm_page.control.${action}`)}
        </Button>
      )}
      {dialog?.action === 'stop' && (
        <StopDialog
          key={dialog.session}
          open={dialog.open}
          swarm={swarm}
          close={close}
          changed={changed}
        />
      )}
      {dialog && dialog.action !== 'stop' && (
        <StartDialog
          key={dialog.session}
          open={dialog.open}
          swarm={swarm}
          projectName={projectName}
          run={dialog.action === 'restart' ? dialog.run : null}
          close={close}
          changed={changed}
          chose={chose}
        />
      )}
    </>
  );
}

/** Отправка действия: повтор, пока идёт запрос, не уходит; отказ остаётся
 * в диалоге причиной сервера; успех закрывает диалог. Страница перечитывает
 * исполнение в обоих случаях: отказ часто означает, что состояние уже другое. */
function useExecutionSubmit(close: () => void, changed: () => Promise<void>) {
  const notify = useNotificationStore((state) => state.showNotification);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState('');
  const lock = useRef(false);
  const submit = async (mutation: () => Promise<unknown>, success: string) => {
    if (lock.current) return;
    lock.current = true;
    setPending(true);
    setError('');
    let done = false;
    try {
      await mutation();
      done = true;
    } catch (failure) {
      setError(errorMessage(failure));
    } finally {
      lock.current = false;
      setPending(false);
    }
    if (done) {
      close();
      notify(success, 'success');
    }
    await changed();
  };
  return { pending, error, submit };
}

function Refusal({ error }: { error: string }) {
  if (!error) return null;
  return (
    <p className={`${styles.notice} ${styles.operationFailure}`} role="alert">
      {error}
    </p>
  );
}

function DialogActions({
  pending,
  close,
  submit,
}: {
  pending: boolean;
  close: () => void;
  submit: ReactNode;
}) {
  const { t } = useTranslation();
  return (
    <div className={styles.confirmActions}>
      <Button type="button" variant="ghost" onClick={close} disabled={pending}>
        {t('common.cancel')}
      </Button>
      {submit}
    </div>
  );
}

/** Запуск роя. Первый запуск выбирает определение из каталога проекта роя,
 * показывает его граф до запуска и передаёт commit, если он введён. Повторный
 * идёт на закреплённом снимке: выбора нет, каталог не читается, commit — тот,
 * что закрепил первый запуск. Роли графа, которые запуск отвергнет, видны
 * до нажатия и описывают кнопку запуска (#241). Каждый запуск несёт задачу
 * прохода (#242): у повторного поле пустое, рядом — задача прошлого прохода.
 * Задачу, которую сервер заведомо отвергнет, диалог не отправляет: причина
 * остаётся у поля, фокус переходит к нему. */
function StartDialog({
  open,
  swarm,
  projectName,
  run,
  close,
  changed,
  chose,
}: {
  open: boolean;
  swarm: AccessSwarm;
  projectName: string;
  run: PipelineRun | null;
  close: () => void;
  changed: () => Promise<void>;
  chose?: (definition: string) => void;
}) {
  const { t } = useTranslation();
  const rolesNote = useId();
  const [chosen, setChosen] = useState('');
  // Commit читается при отправке: панель его не судит, сервер назовёт причину.
  const commit = useRef<HTMLInputElement>(null);
  const [goal, setGoal] = useState('');
  // Пустая задача — отказ только после попытки запуска: до неё поле не ругает.
  const [attempted, setAttempted] = useState(false);
  const goalInput = useRef<HTMLTextAreaElement>(null);
  const problem = goalProblem(goal);
  const shownProblem = problem === 'empty' && !attempted ? null : problem;
  const projectId = swarm.project_id;
  const loadCatalog = useCallback(() => pipelinesApi.listDefinitions(projectId), [projectId]);
  const catalog = useResource(run ? null : loadCatalog, { headerRefresh: false });
  const definition = run ? undefined : definitionChoice(catalog.data, chosen);
  const name = run ? restartDefinition(run) : definition?.name;
  const first = run ? '' : (definition?.name ?? '');
  useEffect(() => {
    if (first) chose?.(first);
  }, [first, chose]);
  const { pending, error, submit } = useExecutionSubmit(close, changed);
  const onSubmit = (event: FormEvent) => {
    event.preventDefault();
    if (!name || pending) return;
    if (problem) {
      setAttempted(true);
      goalInput.current?.focus();
      return;
    }
    const typed = run ? '' : (commit.current?.value ?? '');
    void submit(
      () => executionApi.start(swarm.id, { definition: name, goal, commit: typed }),
      t('swarm_page.control.started', { name: swarm.name })
    );
  };
  const goalField = (
    <GoalField
      input={goalInput}
      value={goal}
      change={setGoal}
      problem={shownProblem}
      disabled={pending}
    />
  );
  const previous = run ? passGoal(run)?.goal : undefined;
  return (
    <Modal
      open={open}
      title={t(run ? 'swarm_page.control.restart_title' : 'swarm_page.control.start_title', {
        name: swarm.name,
      })}
      onClose={close}
      closeDisabled={pending}
      width={760}
    >
      <form className={styles.confirmBody} onSubmit={onSubmit}>
        {run ? (
          <>
            <RestartSummary run={run} />
            {previous && <PreviousGoal goal={previous} />}
            {goalField}
          </>
        ) : (
          <>
            <DefinitionPicker
              projectName={projectName}
              definitions={catalog.data}
              loading={catalog.loading}
              error={catalog.error ?? ''}
              retry={catalog.refresh}
              chosen={definition?.name}
              choose={setChosen}
            />
            {definition && (
              <DefinitionPreview
                key={`${definition.name}:${definition.version}:${definition.revision}`}
                projectId={projectId}
                definition={definition}
                refreshCatalog={catalog.refresh}
              />
            )}
            {goalField}
            <CommitField input={commit} />
          </>
        )}
        {name && (
          <StartRolesNote key={first} id={rolesNote} swarmId={swarm.id} definition={first} />
        )}
        <Refusal error={error} />
        <DialogActions
          pending={pending}
          close={close}
          submit={
            <Button
              type="submit"
              loading={pending}
              disabled={!name}
              aria-describedby={name ? rolesNote : undefined}
            >
              {t('swarm_page.control.submit_start')}
            </Button>
          }
        />
      </form>
    </Modal>
  );
}

function RestartSummary({ run }: { run: PipelineRun }) {
  const { t } = useTranslation();
  const identity = snapshotIdentity(run);
  return (
    <>
      <p>{t('swarm_page.control.restart_text')}</p>
      {identity ? (
        <SnapshotFacts identity={identity} />
      ) : (
        <p>
          {t('swarm_page.snapshot.definition')}: {run.name}
        </p>
      )}
    </>
  );
}

/** Определения каталога проекта роя — радиогруппой: стрелки выбирают,
 * выбор сразу показывает граф. */
function DefinitionPicker({
  projectName,
  definitions,
  loading,
  error,
  retry,
  chosen,
  choose,
}: {
  projectName: string;
  definitions: WorkflowDefinition[] | null;
  loading: boolean;
  error: string;
  retry: () => Promise<void>;
  chosen?: string;
  choose: (name: string) => void;
}) {
  const { t, i18n } = useTranslation();
  const group = useId();
  const hint = useId();
  return (
    <fieldset className={local.picker} aria-describedby={hint}>
      <legend className={styles.fieldLabel}>{t('swarm_page.control.definition')}</legend>
      <p id={hint} className={styles.fieldHint}>
        {t('swarm_page.control.definition_hint', { project: projectName })}
      </p>
      <LoadState loading={loading} error={error} retry={retry}>
        {definitions?.length === 0 ? (
          <p className={local.empty}>{t('swarm_page.control.definitions_empty')}</p>
        ) : (
          <div className={local.options}>
            {definitions?.map((definition) => {
              const published = formatDateTimeValue(definition.publishedAt, i18n.language);
              return (
                <label key={definition.name} className={local.option}>
                  <input
                    type="radio"
                    name={group}
                    value={definition.name}
                    checked={chosen === definition.name}
                    onChange={() => choose(definition.name)}
                  />
                  <span className={local.optionName}>{definition.name}</span>
                  <span className={local.optionMeta}>
                    {t('swarm_page.control.definition_version', { version: definition.version })}
                    {published && ` · ${t('swarm_page.control.published', { time: published })}`}
                  </span>
                </label>
              );
            })}
          </div>
        )}
      </LoadState>
      {definitions && definitions.length > 1 && !chosen && (
        <p className={styles.fieldHint}>{t('swarm_page.control.choose')}</p>
      )}
    </fieldset>
  );
}

/** Граф выбранного определения до запуска — тем же компонентом, что граф
 * роя, только для чтения. Граф запрашивается ровно той версии и ревизии,
 * что в списке (#199): если каталог успел измениться, сервер ответит 409, и
 * повтор перечитает каталог. */
function DefinitionPreview({
  projectId,
  definition,
  refreshCatalog,
}: {
  projectId: string;
  definition: WorkflowDefinition;
  refreshCatalog: () => Promise<void>;
}) {
  const { t } = useTranslation();
  const { name, version, revision } = definition;
  const load = useCallback(
    () => pipelinesApi.getDefinitionGraph(projectId, name, version, revision),
    [projectId, name, version, revision]
  );
  const graph = useResource(load, { headerRefresh: false });
  const retry = async () => {
    await refreshCatalog();
    await graph.refresh();
  };
  const label = t('swarm_page.control.preview', { name, version });
  return (
    <div className={local.preview}>
      <p className={styles.fieldLabel}>{label}</p>
      <LoadState loading={graph.loading} error={graph.error ?? ''} retry={retry}>
        {graph.data?.svg ? (
          <RunGraph svg={graph.data.svg} label={label} />
        ) : (
          <p className={styles.fieldHint}>{t('pipelines.graph_empty')}</p>
        )}
      </LoadState>
    </div>
  );
}

/** Задача прохода (#242): многострочный текст для акторов, обязательный на
 * каждом запуске. Счётчик — в байтах UTF-8, как предел сервера; причина
 * отказа — у поля и в его описании. */
function GoalField({
  input,
  value,
  change,
  problem,
  disabled,
}: {
  input: RefObject<HTMLTextAreaElement | null>;
  value: string;
  change: (goal: string) => void;
  problem: GoalProblem | null;
  disabled: boolean;
}) {
  const { t } = useTranslation();
  const id = useId();
  const hint = useId();
  const counter = useId();
  const error = useId();
  const bytes = goalBytes(value);
  return (
    <div className={styles.field}>
      <label className={styles.fieldLabel} htmlFor={id}>
        {t('swarm_page.control.goal')}
      </label>
      <p id={hint} className={styles.fieldHint}>
        {t('swarm_page.control.goal_hint')}
      </p>
      <textarea
        ref={input}
        id={id}
        name="goal"
        className={local.goal}
        value={value}
        rows={4}
        disabled={disabled}
        autoComplete="off"
        aria-required="true"
        aria-invalid={problem ? true : undefined}
        aria-describedby={`${hint} ${error} ${counter}`}
        onChange={(event) => change(event.target.value)}
      />
      <div className={local.goalMeta}>
        <p id={error} className={styles.fieldError} aria-live="polite">
          {problem && t(`swarm_page.control.goal_problems.${problem}`, { limit: MAX_GOAL_BYTES })}
        </p>
        <p
          id={counter}
          className={local.goalBytes}
          data-over={bytes > MAX_GOAL_BYTES || undefined}
          data-goal-bytes=""
        >
          {t('swarm_page.control.goal_bytes', { bytes, limit: MAX_GOAL_BYTES })}
        </p>
      </div>
    </div>
  );
}

/** Задача прошлого прохода — подсказкой у пустого поля нового: текст как
 * набран, переводы строк сохранены, разметка показана текстом. */
function PreviousGoal({ goal }: { goal: string }) {
  const { t } = useTranslation();
  const labelId = useId();
  return (
    <section className={local.previousGoal} aria-labelledby={labelId} data-previous-goal="">
      <p id={labelId} className={local.snapshotLabel}>
        {t('swarm_page.control.previous_goal')}
      </p>
      <p className={page.goalText} data-goal-text="">
        {goal}
      </p>
    </section>
  );
}

function CommitField({ input }: { input: RefObject<HTMLInputElement | null> }) {
  const { t } = useTranslation();
  const id = useId();
  const hint = useId();
  return (
    <div className={styles.field}>
      <label className={styles.fieldLabel} htmlFor={id}>
        {t('swarm_page.control.commit')}
      </label>
      <input
        ref={input}
        id={id}
        name="commit"
        className={`input ${styles.mono}`}
        defaultValue=""
        autoComplete="off"
        autoCapitalize="off"
        spellCheck={false}
        aria-describedby={hint}
      />
      <p id={hint} className={styles.fieldHint}>
        {t('swarm_page.control.commit_hint')}
      </p>
    </div>
  );
}

/** Остановка роя с подтверждением: продвижение замирает, открытый вопрос
 * оператору отменяется; акторы, их контейнеры и workspace остаются. */
function StopDialog({
  open,
  swarm,
  close,
  changed,
}: {
  open: boolean;
  swarm: AccessSwarm;
  close: () => void;
  changed: () => Promise<void>;
}) {
  const { t } = useTranslation();
  const { pending, error, submit } = useExecutionSubmit(close, changed);
  const onSubmit = (event: FormEvent) => {
    event.preventDefault();
    if (pending) return;
    void submit(
      () => executionApi.stop(swarm.id),
      t('swarm_page.control.stopped', { name: swarm.name })
    );
  };
  return (
    <Modal
      open={open}
      title={t('swarm_page.control.stop_title', { name: swarm.name })}
      onClose={close}
      closeDisabled={pending}
    >
      <form className={styles.confirmBody} onSubmit={onSubmit}>
        <p>{t('swarm_page.control.stop_text')}</p>
        <Refusal error={error} />
        <DialogActions
          pending={pending}
          close={close}
          submit={
            <Button type="submit" variant="danger" loading={pending}>
              {t('swarm_page.control.submit_stop')}
            </Button>
          }
        />
      </form>
    </Modal>
  );
}
