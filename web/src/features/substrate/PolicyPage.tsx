import { useCallback, useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { EmptyState } from '@/components/ui/EmptyState';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';
import { Select } from '@/components/ui/Select';
import { useProjectStore } from '@/stores';
import { BroodApiError } from '@/services/brood/http';
import { policyApi, type AccessPolicy, type Binding } from '@/services/brood/substrate';
import { ChipsInput } from './components/ChipsInput';
import { Field, Section } from './components/form';
import { PageHeader } from './components/PageHeader';
import { Segmented } from './components/Segmented';
import { clone } from './format';
import { useActions, useResource } from './hooks';
import styles from './substrate.module.scss';

/** Roles each scope accepts (cmd/ateapi/internal/apivalidation/access_policy.go):
 * editor exists only inside an atespace. ateapi checks again on save. */
const ROLES = {
  global: ['owner', 'viewer'],
  project: ['owner', 'editor', 'viewer'],
} as const;

type Scope = keyof typeof ROLES;

/** A scope may have no policy of its own: that is data, not an error. */
async function loadPolicy(project: string): Promise<{ policy: AccessPolicy | null }> {
  try {
    return { policy: await policyApi.get(project) };
  } catch (e) {
    if (e instanceof BroodApiError && e.status === 404) return { policy: null };
    throw e;
  }
}

/** Bindings as sent: empty member lists and roles dropped. */
const cleaned = (bindings: Binding[]) =>
  bindings.filter((b) => b.role && b.members?.length).map((b) => ({ role: b.role, members: b.members }));

/** brood: the access policy of the whole install or of the active project --
 * one per scope, replaced wholesale. Saving an existing policy sends back the
 * uid and version it was read at (ateapi's optimistic lock); a scope without
 * one gets it created. */
export function PolicyPage() {
  const { t } = useTranslation();
  const project = useProjectStore((state) => state.project);
  const [scope, setScope] = useState<Scope>(project ? 'project' : 'global');
  const target = scope === 'project' ? project : '';

  useEffect(() => {
    if (!project) setScope('global');
  }, [project]);

  const load = useCallback(() => loadPolicy(target), [target]);
  const { data, error, loading, refresh } = useResource(load);
  const { run, confirm } = useActions(refresh);
  const [draft, setDraft] = useState<Binding[] | null>(null);
  const [saving, setSaving] = useState(false);
  // Предусловие следующего сохранения. После конфликта -- метаданные свежей
  // политики: черновик остаётся, и сохранение заменит чужую правку
  // осознанно, после предупреждения.
  const [base, setBase] = useState<AccessPolicy['metadata']>(undefined);
  const [conflict, setConflict] = useState(false);

  const policy = data?.policy ?? null;
  const loaded = policy?.bindings ?? [];
  // A draft exists while editing: always for an existing policy, after
  // "Create" for a scope without one.
  useEffect(() => {
    setDraft(policy ? clone(loaded) : null);
    setBase(policy?.metadata);
    setConflict(false);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [data]);

  const roles = ROLES[scope];
  const bindings = draft ?? [];
  const dirty = draft !== null && JSON.stringify(cleaned(draft)) !== JSON.stringify(cleaned(loaded));
  const members = loaded.reduce((sum, b) => sum + (b.members?.length ?? 0), 0);

  const setBinding = (index: number, patch: Partial<Binding>) =>
    setDraft((prev) => (prev ?? []).map((b, i) => (i === index ? { ...b, ...patch } : b)));

  const update = async (body: AccessPolicy) => {
    try {
      return await policyApi.update(target, body);
    } catch (e) {
      // 409: политику поменяли после чтения. Берём её новые предусловия,
      // черновик не трогаем -- решать, сохранять ли поверх, пользователю.
      if (e instanceof BroodApiError && e.status === 409) {
        setBase((await policyApi.get(target)).metadata);
        setConflict(true);
      }
      throw e;
    }
  };

  const save = async () => {
    if (draft === null) return;
    setSaving(true);
    const body: AccessPolicy = { metadata: base, bindings: cleaned(draft) };
    try {
      await run(
        () => (policy ? update(body) : policyApi.create(target, body)),
        policy ? t('policy.saved') : t('policy.created')
      );
    } finally {
      setSaving(false);
    }
  };

  const remove = () =>
    confirm({
      title: t('policy.delete_title'),
      message: t('policy.delete_message', { project }),
      confirmText: t('substrate.delete'),
      success: t('policy.deleted'),
      run: () => policyApi.remove(project),
    });

  const scopeLabel = scope === 'project' ? t('policy.scope_project', { project }) : t('policy.scope_global');

  return (
    <div className={styles.page}>
      <PageHeader
        title={t('policy.title')}
        meta={[
          scopeLabel,
          policy && t('policy.meta_bindings', { count: loaded.length }),
          policy && t('policy.meta_members', { count: members }),
          data && !policy && t('policy.meta_none'),
        ]}
        actions={
          scope === 'project' &&
          policy && (
            <Button variant="ghost" className={styles.dangerText} onClick={remove}>
              {t('policy.delete')}
            </Button>
          )
        }
      />

      <div className={styles.toolbar}>
        <Segmented
          value={scope}
          ariaLabel={t('policy.scope')}
          onChange={setScope}
          options={[
            { value: 'global', label: t('policy.scope_global') },
            {
              value: 'project',
              label: project ? t('policy.scope_project', { project }) : t('policy.scope_project_none'),
              disabled: !project,
            },
          ]}
        />
      </div>

      <p className={styles.notice}>{t(`policy.roles_hint_${scope}`)}</p>

      {loading ? (
        <LoadingSpinner />
      ) : error ? (
        <div className={styles.empty}>
          <EmptyState title={t('substrate.load_failed')} description={error} />
        </div>
      ) : draft === null ? (
        <div className={styles.empty}>
          <EmptyState
            title={t('policy.missing')}
            description={t(`policy.missing_hint_${scope}`)}
            action={
              <Button onClick={() => setDraft([{ role: roles[roles.length - 1], members: [] }])}>
                {t('policy.create')}
              </Button>
            }
          />
        </div>
      ) : (
        <>
          <Section
            title={t('policy.bindings')}
            hint={t('policy.bindings_hint')}
            action={
              <Button
                variant="secondary"
                size="sm"
                onClick={() =>
                  setDraft((prev) => [...(prev ?? []), { role: roles[roles.length - 1], members: [] }])
                }
              >
                {t('policy.add_binding')}
              </Button>
            }
          >
            {bindings.length === 0 && <p className={styles.notice}>{t('policy.no_bindings')}</p>}
            {bindings.map((binding, index) => (
              <div key={index} className={styles.card}>
                <div className={styles.cardHead}>
                  <h4 className={styles.cardTitle}>{t('policy.binding_n', { n: index + 1 })}</h4>
                  <Button
                    variant="ghost"
                    size="sm"
                    className={styles.dangerText}
                    onClick={() => setDraft((prev) => (prev ?? []).filter((_, i) => i !== index))}
                  >
                    {t('substrate.delete')}
                  </Button>
                </div>
                <div className={styles.grid2}>
                  <Field label={t('policy.role')} hint={t(`policy.role_hint_${binding.role}`, '')}>
                    {(id) => (
                      <Select
                        id={id}
                        value={binding.role}
                        fullWidth
                        options={roles.map((role) => ({ value: role, label: t(`policy.role_${role}`) }))}
                        onChange={(role) => setBinding(index, { role })}
                      />
                    )}
                  </Field>
                  <Field label={t('policy.members')} hint={t('policy.members_hint')}>
                    {(id) => (
                      <ChipsInput
                        id={id}
                        unique
                        values={binding.members ?? []}
                        placeholder="user:system:serviceaccount:team:alice"
                        onChange={(values) => setBinding(index, { members: values })}
                      />
                    )}
                  </Field>
                </div>
              </div>
            ))}
          </Section>

          {conflict && <p className={styles.notice}>{t('policy.conflict')}</p>}

          <div className={styles.saveBar}>
            <span className={styles.saveBarHint}>
              {dirty || !policy ? t('policy.unsaved') : t('policy.in_sync')}
            </span>
            {policy && (
              // Перечитать, а не вернуть прочитанное: после конфликта на сервере уже другая политика.
              <Button variant="secondary" disabled={!dirty || saving} onClick={() => void refresh()}>
                {t('policy.revert')}
              </Button>
            )}
            {!policy && (
              <Button variant="secondary" disabled={saving} onClick={() => setDraft(null)}>
                {t('common.cancel')}
              </Button>
            )}
            <Button onClick={() => void save()} loading={saving} disabled={policy ? !dirty : false}>
              {policy ? t('substrate.save') : t('policy.create')}
            </Button>
          </div>
        </>
      )}
    </div>
  );
}
