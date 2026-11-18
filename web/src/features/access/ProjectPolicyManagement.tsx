import { useCallback, useId, useRef, useState, type FormEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { errorMessage } from '@/services/brood/http';
import {
  canEditProjectPolicy,
  projectOwnershipApi,
  type ProjectOwnership,
} from '@/services/brood/projectOwnership';
import { LoadState, useAccessResource, useAdministrator } from './shared';
import { ProjectWorkflowPolicy } from './ProjectWorkflowPolicy';
import styles from '@/features/substrate/substrate.module.scss';

export interface ProjectOwnershipPanelProps {
  ownership: ProjectOwnership | undefined;
  loading: boolean;
  error: string;
  administrator: boolean;
  busy: boolean;
  submit: (username: string) => void;
  retry: () => Promise<void>;
}

export function ProjectOwnershipPanel({
  ownership,
  loading,
  error,
  administrator,
  busy,
  submit,
  retry,
}: ProjectOwnershipPanelProps) {
  const { t } = useTranslation();
  const id = useId();
  const save = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!administrator || busy || !ownership || error || loading) return;
    const field = event.currentTarget.elements.namedItem('username') as HTMLInputElement;
    submit(field.value);
  };
  return (
    <section className={styles.section}>
      <h2 className={styles.sectionTitle}>{t('project_ownership.title')}</h2>
      <p className={styles.sectionHint}>{t('project_ownership.hint')}</p>
      <LoadState loading={loading} error={error} retry={retry}>
        {ownership && (
          <>
            <dl className={styles.details}>
              <dt>{t('project_ownership.current')}</dt>
              <dd>{ownership.ownerUsername || t('project_ownership.unassigned')}</dd>
              <dt>{t('project_ownership.revision')}</dt>
              <dd>{ownership.revision}</dd>
            </dl>
            {administrator ? (
              <form
                key={`${ownership.projectId}:${ownership.revision}`}
                className={styles.form}
                onSubmit={save}
              >
                <label className={styles.field} htmlFor={id}>
                  <span className={styles.fieldLabel}>{t('project_ownership.username')}</span>
                  <input
                    id={id}
                    className="input"
                    name="username"
                    required
                    maxLength={256}
                    autoComplete="off"
                    defaultValue={ownership.ownerUsername}
                    disabled={busy}
                  />
                </label>
                <div className={styles.actions}>
                  <Button type="submit" disabled={busy}>
                    {t('project_ownership.assign')}
                  </Button>
                  <Button
                    type="button"
                    variant="secondary"
                    disabled={busy || !ownership.ownerUid}
                    onClick={() => submit('')}
                  >
                    {t('project_ownership.clear')}
                  </Button>
                </div>
              </form>
            ) : (
              <p className={styles.sectionHint}>{t('project_ownership.assignment_hint')}</p>
            )}
          </>
        )}
      </LoadState>
    </section>
  );
}

export function ProjectPolicyManagement({ projectId }: { projectId: string }) {
  const administrator = useAdministrator();
  const load = useCallback(() => projectOwnershipApi.get(projectId), [projectId]);
  const resource = useAccessResource(load);
  const ownership = resource.data ?? undefined;
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState('');
  const lock = useRef(false);
  const submit = async (username: string) => {
    if (lock.current || !administrator || !ownership || resource.error || resource.loading) return;
    lock.current = true;
    setBusy(true);
    setRefusal('');
    try {
      await projectOwnershipApi.set(projectId, ownership.revision, username);
    } catch (error) {
      setRefusal(errorMessage(error));
    } finally {
      await resource.refresh();
      lock.current = false;
      setBusy(false);
    }
  };
  const canEdit = canEditProjectPolicy(
    administrator,
    ownership?.viewerUid ?? '',
    ownership,
    Boolean(resource.error)
  );
  return (
    <>
      <ProjectOwnershipPanel
        ownership={ownership}
        loading={resource.loading}
        error={resource.error}
        administrator={administrator}
        busy={busy}
        submit={(username) => void submit(username)}
        retry={resource.refresh}
      />
      {refusal && (
        <p className={styles.operationFailure} role="alert">
          {refusal}
        </p>
      )}
      <ProjectWorkflowPolicy projectId={projectId} canEdit={canEdit} />
    </>
  );
}
