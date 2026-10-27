import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Input } from '@/components/ui/Input';
import { Select } from '@/components/ui/Select';
import type { Actor } from '@/services/brood/substrate';
import { EditorSheet } from './components/EditorSheet';
import { Field, Section } from './components/form';
import { KeyValueEditor } from './components/KeyValueEditor';
import { StateBadge } from './components/StateBadge';
import { clone, DASH, formatDate, fromPairs, isShortName, shortState, toPairs, type Pair } from './format';
import styles from './substrate.module.scss';

interface ActorEditorProps {
  project: string;
  /** null -- create a new actor; otherwise edit this one. */
  actor: Actor | null;
  /** The project's template names; null while the page is still loading. */
  templates: string[] | null;
  open: boolean;
  onClose: () => void;
  /** Resolves true once saved; on a refusal the form stays as typed. */
  onSubmit: (actor: Actor) => Promise<boolean>;
}

/** brood: create or edit an actor. ateapi lets an update change the template
 * (only while the actor is suspended) and the worker selector; the update
 * replaces the whole object, so the edit is applied onto the actor as read and
 * the rest (sourceTag, metadata preconditions) goes back unchanged. */
export function ActorEditor({ project, actor, templates, open, onClose, onSubmit }: ActorEditorProps) {
  const { t, i18n } = useTranslation();
  const editing = actor !== null;
  const state = shortState(actor?.status?.state, 'ACTOR_STATE_');
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

  const nameValid = editing || isShortName(name);
  const valid = nameValid && template !== '';
  const dirty =
    name !== initial.name ||
    template !== initial.template ||
    sourceTag !== initial.sourceTag ||
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
    return {
      metadata: { name },
      actorTemplate: { atespace: project, name: template },
      workerSelector: selector,
      sourceTag: sourceTag ? { atespace: project, name: sourceTag } : undefined,
    };
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

  const templateOptions = (templates ?? []).map((value) => ({ value, label: value }));
  if (template && !templateOptions.some((o) => o.value === template)) {
    templateOptions.unshift({ value: template, label: template });
  }

  return (
    <EditorSheet
      open={open}
      onClose={onClose}
      size="lg"
      eyebrow={project}
      title={editing ? t('actors.edit_title', { name: actor.metadata.name }) : t('actors.create_title')}
      description={editing ? undefined : t('actors.create_hint')}
      onSave={() => void save()}
      saveLabel={editing ? t('substrate.save') : t('substrate.create')}
      canSave={valid && (dirty || !editing)}
      saving={saving}
      dirty={dirty}
    >
      <div className={styles.form}>
        {editing && (
          <Section title={t('actors.status_section')}>
            <dl className={styles.details}>
              <dt>{t('substrate.state')}</dt>
              <dd>
                <StateBadge state={state} namespace="actors.state" />
              </dd>
              <dt>{t('actors.node')}</dt>
              <dd>{actor.status?.assignedNode || actor.status?.workerAssignment?.nodeName || DASH}</dd>
              <dt>{t('actors.worker')}</dt>
              <dd className={styles.mono}>{actor.status?.workerAssignment?.worker?.name || DASH}</dd>
              <dt>{t('actors.snapshots')}</dt>
              <dd>{actor.status?.snapshots?.length ?? 0}</dd>
              {actor.sourceTag && (
                <>
                  <dt>{t('actors.source_tag')}</dt>
                  <dd className={styles.mono}>{actor.sourceTag.name}</dd>
                </>
              )}
              <dt>{t('substrate.updated')}</dt>
              <dd>{formatDate(actor.metadata.updateTime, i18n.language)}</dd>
              <dt>UID</dt>
              <dd className={styles.mono}>{actor.metadata.uid}</dd>
            </dl>
            {actor.status?.crash && (
              <pre className={styles.code}>{JSON.stringify(actor.status.crash, null, 2)}</pre>
            )}
          </Section>
        )}

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
                placeholder={templates === null ? t('common.loading') : t('actors.template_pick')}
                disabled={templateLocked || templates === null}
                onChange={setTemplate}
                fullWidth
              />
            )}
          </Field>
          {!editing && (
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
      </div>
    </EditorSheet>
  );
}
