import { useEffect, useMemo, useState, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Input } from '@/components/ui/Input';
import { Select } from '@/components/ui/Select';
import { accessApi } from '@/services/brood/access';
import { useAdministrator } from '@/features/access/shared';
import { RelatedLink } from '@/features/objects/ObjectLink';
import { actorPool, actorWorker } from '@/features/objects/relations';
import { errorMessage } from '@/services/brood/http';
import type { Actor, ActorTemplate } from '@/services/brood/substrate';
import { actorCreateDraft, actorState } from './actorLifecycle';
import { originProblem } from './originDraft';
import { ManagedProfile } from './components/ManagedProfile';
import { EditorSheet } from './components/EditorSheet';
import { PanelActions, type PanelActionGroup } from './components/EntityPanel';
import { Field, Section } from './components/form';
import { KeyValueEditor } from './components/KeyValueEditor';
import { StateBadge } from './components/StateBadge';
import { clone, DASH, formatDate, fromPairs, isShortName, toPairs, type Pair } from './format';
import styles from './substrate.module.scss';

interface ActorEditorProps {
  /** ID роя: в него уходят запросы. */
  project: string;
  /** Имя роя в заголовке листа (#195): ID -- служебный. */
  swarmName?: string;
  /** Проект роя: из его repository подставляется origin нового managed-актора. */
  projectId?: string;
  /** null -- create a new actor; otherwise edit this one. */
  actor: Actor | null;
  /** Шаблоны роя, включая виртуальные профили; null во время загрузки. */
  templates: ActorTemplate[] | null;
  open: boolean;
  onClose: () => void;
  /** Resolves true once saved; on a refusal the form stays as typed. */
  onSubmit: (actor: Actor) => Promise<boolean>;
  /** Действия над существующим актором (#157): обычные — над формой,
   * необратимые — под ней. Редактор native-актора и есть его панель. */
  actions?: PanelActionGroup[];
  /** Отказ последнего действия над этим актором: страница под панелью скрыта. */
  notice?: ReactNode;
  /** Связи существующего актора (#195): рой и шаблон ссылками. */
  relations?: ReactNode;
}

/** brood: create or edit an actor. ateapi lets an update change the template
 * (only while the actor is suspended) and the worker selector; the update
 * replaces the whole object, so the edit is applied onto the actor as read and
 * the rest (sourceTag, metadata preconditions) goes back unchanged. For an
 * existing actor the sheet is its side panel: state and actions on top, as in
 * EntityPanel, then the form, the irreversible block last. */
export function ActorEditor({
  project,
  swarmName,
  projectId,
  actor,
  templates,
  open,
  onClose,
  onSubmit,
  actions = [],
  notice,
  relations,
}: ActorEditorProps) {
  const { t, i18n } = useTranslation();
  const administrator = useAdministrator();
  const editing = actor !== null;
  const managed = actor?.provider === 'proxmox';
  const state = actor ? actorState(actor) : '';
  const templateLocked = editing && state !== 'suspended';

  const initial = useMemo(
    () => ({
      name: actor?.metadata.name ?? '',
      template: actor?.actorTemplate?.name ?? '',
      labels: fromPairs(toPairs(actor?.workerSelector?.matchLabels)),
      sourceTag: actor?.sourceTag?.name ?? '',
    }),
    [actor]
  );
  const [name, setName] = useState(initial.name);
  const [template, setTemplate] = useState(initial.template);
  const [labels, setLabels] = useState<Record<string, string>>(initial.labels);
  const [sourceTag, setSourceTag] = useState(initial.sourceTag);
  const [saving, setSaving] = useState(false);
  // Origin нового managed-актора: null — не трогали, поле следует repository
  // проекта; сбой чтения repository не мешает ввести origin вручную.
  const [origin, setOrigin] = useState<string | null>(null);
  const [repository, setRepository] = useState<{ origin: string } | { error: string } | null>(
    null
  );
  useEffect(() => {
    if (editing || !projectId) return;
    let current = true;
    accessApi.repository(projectId).then(
      (value) => current && setRepository({ origin: value }),
      (reason: unknown) => current && setRepository({ error: errorMessage(reason) })
    );
    return () => {
      current = false;
    };
  }, [editing, projectId]);
  const projectOrigin = repository && 'origin' in repository ? repository.origin : '';
  const originValue = origin ?? projectOrigin;

  const selectedTemplate = templates?.find((tpl) => tpl.metadata.name === template);
  const managedTemplate = selectedTemplate?.provider === 'proxmox';
  const originError = !editing && managedTemplate ? originProblem(originValue) : null;
  const nameValid = editing || isShortName(name);
  const valid =
    nameValid && template !== '' && !managed && (editing || !!selectedTemplate) && !originError;
  const dirty =
    name !== initial.name ||
    template !== initial.template ||
    sourceTag !== initial.sourceTag ||
    origin !== null ||
    JSON.stringify(labels) !== JSON.stringify(initial.labels);

  const build = (): Actor => {
    const selector = Object.keys(labels).length ? { matchLabels: labels } : undefined;
    if (actor) {
      const next = clone(actor);
      delete next.status;
      next.actorTemplate = { atespace: project, name: template };
      next.workerSelector = selector;
      return next;
    }
    return actorCreateDraft(
      project,
      name,
      selectedTemplate!,
      labels,
      sourceTag,
      managedTemplate ? originValue : ''
    );
  };

  const save = async () => {
    if (!valid) return;
    setSaving(true);
    try {
      await onSubmit(build());
    } finally {
      setSaving(false);
    }
  };

  const templateOptions = (templates ?? [])
    .filter((tpl) => !editing || tpl.provider !== 'proxmox')
    .map((tpl) => ({
      value: tpl.metadata.name,
      label: tpl.provider === 'proxmox' ? `${tpl.metadata.name} · Proxmox` : tpl.metadata.name,
    }));
  if (template && !templateOptions.some((o) => o.value === template)) {
    templateOptions.unshift({ value: template, label: template });
  }

  return (
    <EditorSheet
      open={open}
      onClose={onClose}
      size="lg"
      eyebrow={swarmName ?? project}
      title={
        editing
          ? t(managed ? 'actors.details_title' : 'actors.edit_title', { name: actor.metadata.name })
          : t('actors.create_title')
      }
      description={editing ? undefined : t('actors.create_hint')}
      onSave={managed ? undefined : () => void save()}
      saveLabel={editing ? t('substrate.save') : t('substrate.create')}
      canSave={valid && (dirty || !editing)}
      saving={saving}
      dirty={dirty}
    >
      <div className={styles.form}>
        {editing && (
          <div className={styles.panelStatus}>
            <StateBadge state={state} namespace={managed ? 'actors.managed_state' : 'actors.state'} />
          </div>
        )}
        <PanelActions groups={actions.filter((group) => !group.danger)} />
        {notice}
        {editing && (
          <Section title={t('actors.status_section')}>
            <dl className={styles.details}>
              {!managed && (
                <>
                  <dt>{t('actors.node')}</dt>
                  <dd>
                    {actor.status?.assignedNode || actor.status?.workerAssignment?.nodeName || DASH}
                  </dd>
                  {/* Воркер и пул -- из status.workerAssignment: у suspended
                      и crashed назначения нет. Ссылка -- администратору. */}
                  <dt>{t('actors.worker')}</dt>
                  <dd className={styles.mono}>
                    <RelatedLink related={actorWorker(actor, administrator)} />
                  </dd>
                  <dt>{t('relations.worker_pool')}</dt>
                  <dd className={styles.mono}>
                    <RelatedLink related={actorPool(actor, administrator)} />
                  </dd>
                  <dt>{t('actors.snapshots')}</dt>
                  <dd>{actor.status?.snapshots?.length ?? 0}</dd>
                </>
              )}
              {actor.sourceTag && (
                <>
                  <dt>{t('actors.source_tag')}</dt>
                  <dd className={styles.mono}>{actor.sourceTag.name}</dd>
                </>
              )}
              <dt>{t('substrate.updated')}</dt>
              <dd>{formatDate(actor.metadata.updateTime, i18n.language)}</dd>
            </dl>
            {managed && <ManagedProfile entity={actor} />}
            {actor.status?.crash && (
              <pre className={styles.code}>{JSON.stringify(actor.status.crash, null, 2)}</pre>
            )}
          </Section>
        )}
        {editing && relations}

        {!managed && (
          <>
            <Section title={t('actors.spec_section')}>
              {!editing && (
                <Input
                  name="actor-name"
                  label={t('substrate.name')}
                  value={name}
                  autoComplete="off"
                  placeholder="luna"
                  hint={t('substrate.name_hint')}
                  error={name && !nameValid ? t('substrate.name_invalid') : undefined}
                  onChange={(event) => setName(event.target.value.trim())}
                />
              )}
              <Field
                label={t('actors.template')}
                hint={
                  templateLocked
                    ? t('actors.template_locked')
                    : templates?.length === 0
                      ? t('actors.no_templates')
                      : t('actors.template_hint')
                }
              >
                {(id) => (
                  <Select
                    id={id}
                    value={template}
                    options={templateOptions}
                    placeholder={
                      templates === null ? t('common.loading') : t('actors.template_pick')
                    }
                    disabled={templateLocked || templates === null}
                    onChange={setTemplate}
                    fullWidth
                  />
                )}
              </Field>
              {!editing && managedTemplate && (
                <Input
                  name="actor-origin"
                  label={t('actors.origin')}
                  value={originValue}
                  inputMode="url"
                  autoComplete="off"
                  spellCheck={false}
                  placeholder="https://git.example.com/team/repo.git"
                  hint={
                    <>
                      {t('actors.origin_hint')}{' '}
                      {repository === null
                        ? ''
                        : 'error' in repository
                          ? t('actors.origin_repository_unavailable', { reason: repository.error })
                          : repository.origin
                            ? t('actors.origin_repository', { origin: repository.origin })
                            : t('actors.origin_no_repository')}
                    </>
                  }
                  error={originError ? t(`actors.origin_invalid.${originError}`) : undefined}
                  onChange={(event) => setOrigin(event.target.value)}
                />
              )}
              {!editing && !managedTemplate && (
                <Input
                  name="actor-source-tag"
                  label={t('actors.source_tag')}
                  value={sourceTag}
                  autoComplete="off"
                  hint={t('actors.source_tag_hint')}
                  onChange={(event) => setSourceTag(event.target.value.trim())}
                />
              )}
            </Section>

            {managedTemplate && <ManagedProfile entity={selectedTemplate!} />}
            <Section title={t('substrate.placement')} hint={t('substrate.placement_hint')}>
              <KeyValueEditor
                key={actor?.metadata.uid ?? 'new'}
                initial={toPairs(initial.labels)}
                onChange={(pairs: Pair[]) => setLabels(fromPairs(pairs))}
                keyPlaceholder={t('substrate.label_key')}
                valuePlaceholder={t('substrate.label_value')}
                addLabel={t('substrate.add_label')}
              />
            </Section>
          </>
        )}
        <PanelActions groups={actions.filter((group) => group.danger)} />
      </div>
    </EditorSheet>
  );
}
