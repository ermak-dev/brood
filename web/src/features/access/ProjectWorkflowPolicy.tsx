import { useCallback, useId, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { stringify } from 'yaml';
import { Button } from '@/components/ui/Button';
import { EditorSheet } from '@/features/substrate/components/EditorSheet';
import { errorMessage } from '@/services/brood/http';
import {
  parseWorkflowPolicyDraft,
  workflowPolicyApi,
  type ProjectWorkflowPolicy as Policy,
} from '@/services/brood/workflowPolicy';
import { LoadState, useAccessResource } from './shared';
import styles from '@/features/substrate/substrate.module.scss';
import local from './ProjectWorkflowPolicy.module.scss';

export function ProjectWorkflowPolicy({
  projectId,
  canEdit,
}: {
  projectId: string;
  canEdit: boolean;
}) {
  const { t } = useTranslation();
  const load = useCallback(() => workflowPolicyApi.get(projectId), [projectId]);
  const resource = useAccessResource(load);
  const [editing, setEditing] = useState<Policy | null>(null);
  return (
    <section className={styles.section}>
      <div className={styles.sectionHead}>
        <h2 className={styles.sectionTitle}>{t('workflow_policy.title')}</h2>
        {canEdit && resource.data && (
          <Button variant="secondary" onClick={() => setEditing(resource.data)}>
            {t('workflow_policy.edit')}
          </Button>
        )}
      </div>
      <p className={styles.sectionHint}>{t('workflow_policy.scope')}</p>
      <LoadState {...resource} retry={resource.refresh}>
        {resource.data && (
          <>
            <dl className={styles.details}>
              <dt>{t('workflow_policy.revision')}</dt>
              <dd>{resource.data.revision}</dd>
            </dl>
            <pre className={`${local.rules} ${styles.mono}`}>{stringify(resource.data.rules)}</pre>
          </>
        )}
      </LoadState>
      {editing && canEdit && (
        <WorkflowPolicyEditor
          policy={editing}
          close={() => setEditing(null)}
          saved={resource.refresh}
        />
      )}
    </section>
  );
}

export function WorkflowPolicyEditor({
  policy,
  close,
  saved,
}: {
  policy: Policy;
  close: () => void;
  saved: () => Promise<void>;
}) {
  const { t } = useTranslation();
  const hintId = useId();
  const invalidId = useId();
  const initial = stringify(policy.rules);
  const [draft, setDraft] = useState(initial);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  const parsed = parseWorkflowPolicyDraft(draft);
  const dirty = draft !== initial;
  const save = async () => {
    if (parsed === null || saving || !dirty) return;
    setSaving(true);
    setError('');
    try {
      await workflowPolicyApi.set(policy.projectId, policy.revision, parsed);
      await saved();
      close();
    } catch (failure) {
      setError(errorMessage(failure));
    } finally {
      setSaving(false);
    }
  };
  return (
    <EditorSheet
      open
      title={t('workflow_policy.edit')}
      description={t('workflow_policy.scope')}
      onClose={close}
      onSave={() => void save()}
      canSave={parsed !== null && dirty}
      saving={saving}
      dirty={dirty}
    >
      <form
        className={styles.form}
        onSubmit={(event) => {
          event.preventDefault();
          void save();
        }}
      >
        <p>
          {t('workflow_policy.revision')}: {policy.revision}
        </p>
        <label className={styles.field}>
          <span className={styles.fieldLabel}>{t('workflow_policy.rules')}</span>
          <textarea
            className={`input ${styles.mono}`}
            rows={14}
            value={draft}
            spellCheck={false}
            disabled={saving}
            aria-invalid={parsed === null}
            aria-describedby={parsed === null ? `${hintId} ${invalidId}` : hintId}
            onChange={(event) => setDraft(event.target.value)}
          />
        </label>
        <p id={hintId} className={styles.fieldHint}>
          {t('workflow_policy.rules_hint')}
        </p>
        <details>
          <summary>{t('workflow_policy.example')}</summary>
          <pre className={`${local.rules} ${styles.mono}`}>
            {'- id: protected-code\n  paths: ["go/**"]\n  require: human'}
          </pre>
        </details>
        {parsed === null && (
          <p id={invalidId} role="alert" className={styles.fieldError}>
            {t('workflow_policy.invalid')}
          </p>
        )}
        {error && (
          <p role="alert" className={`${styles.notice} ${styles.operationFailure}`}>
            {error}
          </p>
        )}
        <button type="submit" hidden disabled={saving || parsed === null || !dirty}>
          {t('substrate.save')}
        </button>
      </form>
    </EditorSheet>
  );
}
