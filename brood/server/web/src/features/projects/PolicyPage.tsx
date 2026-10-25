import { useCallback, useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { EmptyState } from '@/components/ui/EmptyState';
import { Input } from '@/components/ui/Input';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';
import { useNotificationStore, useProjectStore } from '@/stores';
import { useHeaderRefresh } from '@/hooks/useHeaderRefresh';
import {
  deletePolicy,
  getPolicy,
  savePolicy,
  type BroodBinding,
  type BroodPolicy,
} from '@/services/brood/projects';
import styles from './projects.module.scss';

/** brood: политика доступа скоупа -- глобального или активного проекта.
 * Bindings: роль + участники-принципалы. Замена целиком: шлём прочитанные
 * uid и version (оптимистичная блокировка ateapi). */
export function PolicyPage() {
  const project = useProjectStore((state) => state.project);
  const { t } = useTranslation();
  const showNotification = useNotificationStore((state) => state.showNotification);
  const showConfirmation = useNotificationStore((state) => state.showConfirmation);
  const [policy, setPolicy] = useState<BroodPolicy | null>(null);
  const [bindings, setBindings] = useState<BroodBinding[]>([]);
  const [missing, setMissing] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [memberDraft, setMemberDraft] = useState<Record<number, string>>({});

  const scope = project || undefined;
  const roles = project
    ? ['owner', 'editor', 'viewer']
    : ['owner', 'viewer'];

  const refresh = useCallback(async () => {
    if (project === '') {
      // Глобальная политика есть всегда; проект может быть без своей.
    }
    try {
      const loaded = await getPolicy(scope);
      setPolicy(loaded);
      setBindings(loaded.bindings.map((b) => ({ ...b, members: [...b.members] })));
      setMissing(false);
      setError(null);
    } catch (e) {
      const status = (e as { status?: number }).status;
      if (status === 404) {
        setPolicy(null);
        setBindings([]);
        setMissing(true);
        setError(null);
      } else {
        setError(e instanceof Error ? e.message : String(e));
      }
    }
  }, [project, scope]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  useHeaderRefresh(refresh);

  const save = useCallback(async () => {
    if (!policy) return;
    try {
      const saved = await savePolicy({ ...policy, bindings }, scope);
      setPolicy(saved);
      setBindings(saved.bindings.map((b) => ({ ...b, members: [...b.members] })));
      showNotification(t('policy.saved'), 'success');
    } catch (e) {
      showNotification(e instanceof Error ? e.message : String(e), 'error');
    }
  }, [bindings, policy, scope, showNotification, t]);

  const remove = useCallback(() => {
    if (!project) return;
    showConfirmation({
      title: t('policy.delete_title'),
      message: t('policy.delete_message', { project }),
      variant: 'danger',
      confirmText: t('projects.delete_confirm'),
      onConfirm: async () => {
        try {
          await deletePolicy(project);
          showNotification(t('policy.deleted'), 'success');
          await refresh();
        } catch (e) {
          showNotification(e instanceof Error ? e.message : String(e), 'error');
        }
      },
    });
  }, [project, refresh, showConfirmation, showNotification, t]);

  const setRole = (index: number, role: string) => {
    setBindings((prev) => prev.map((b, i) => (i === index ? { ...b, role } : b)));
  };

  const addMember = (index: number) => {
    const member = (memberDraft[index] || '').trim();
    if (!member) return;
    setBindings((prev) =>
      prev.map((b, i) =>
        i === index && !b.members.includes(member)
          ? { ...b, members: [...b.members, member] }
          : b
      )
    );
    setMemberDraft((prev) => ({ ...prev, [index]: '' }));
  };

  const removeMember = (index: number, member: string) => {
    setBindings((prev) =>
      prev.map((b, i) => (i === index ? { ...b, members: b.members.filter((m) => m !== member) } : b))
    );
  };

  if (error) {
    return (
      <div className={styles.page}>
        <EmptyState title={t('projects.load_failed')} description={error} />
      </div>
    );
  }
  if (policy === null && !missing && !error) return <LoadingSpinner />;

  return (
    <div className={styles.page}>
      <div className={styles.header}>
        <div>
          <h1 className={styles.title}>
            {project ? t('policy.title_project', { project }) : t('policy.title_global')}
          </h1>
          <p className={styles.description}>{t('policy.description')}</p>
        </div>
        <div className={styles.actions}>
          {project && (
            <Button variant="danger" onClick={remove}>
              {t('policy.delete')}
            </Button>
          )}
          <Button onClick={() => void save()} disabled={!policy}>
            {t('policy.save')}
          </Button>
        </div>
      </div>

      {missing ? (
        <EmptyState
          title={t('policy.missing_title')}
          description={t('policy.missing_description')}
          action={
            <Button
              onClick={() => {
                setPolicy({
                  name: 'default', uid: '', version: 0,
                  bindings: [], create_time: '', update_time: '',
                });
                setBindings([{ role: 'viewer', members: [] }]);
                setMissing(false);
              }}
            >
              {t('policy.create_first')}
            </Button>
          }
        />
      ) : (
        <div className={styles.bindings}>
          {bindings.length === 0 && (
            <Button
              variant="secondary"
              onClick={() => setBindings([{ role: roles[0], members: [] }])}
            >
              {t('policy.add_binding')}
            </Button>
          )}
          {bindings.map((binding, index) => (
            <div key={index} className={styles.binding}>
              <div className={styles.bindingHead}>
                <label>
                  <span className={styles.fieldLabel}>{t('policy.role')}</span>
                  <select
                    value={binding.role}
                    onChange={(event) => setRole(index, event.target.value)}
                    className="select-trigger"
                  >
                    {roles.map((role) => (
                      <option key={role} value={role}>
                        {t(`policy.role_${role}`)}
                      </option>
                    ))}
                  </select>
                </label>
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => setBindings((prev) => prev.filter((_, i) => i !== index))}
                >
                  {t('projects.delete')}
                </Button>
              </div>
              <div className={styles.members}>
                {binding.members.map((member) => (
                  <span key={member} className="status-badge">
                    {member}
                    <button
                      type="button"
                      className={styles.memberRemove}
                      aria-label={t('policy.remove_member')}
                      onClick={() => removeMember(index, member)}
                    >
                      ×
                    </button>
                  </span>
                ))}
              </div>
              <div className={styles.memberAdd}>
                <Input
                  name={`policy-member-${index}`}
                  placeholder={t('policy.member_placeholder')}
                  value={memberDraft[index] || ''}
                  onChange={(event) =>
                    setMemberDraft((prev) => ({ ...prev, [index]: event.target.value }))
                  }
                  onKeyDown={(event) => {
                    if (event.key === 'Enter') {
                      event.preventDefault();
                      addMember(index);
                    }
                  }}
                />
                <Button variant="secondary" size="sm" onClick={() => addMember(index)}>
                  {t('policy.add_member')}
                </Button>
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
