import { useCallback, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { Select } from '@/components/ui/Select';
import { accessApi, type AccessProject, type GitRemote } from '@/services/brood/access';
import { Field } from '@/features/substrate/components/form';
import styles from '@/features/substrate/substrate.module.scss';
import { LoadState, useAccessActions, useAccessResource } from './shared';
import { repositoryOptions } from './state';

/** brood: repository проекта — origin новых акторов по умолчанию, клон через
 * Git gateway сервера. Выбирает администратор из remotes установки; токены не
 * покидают сервер и сюда не приходят. Участник проекта видит выбор только
 * для чтения. */
export function ProjectRepository({ project, admin }: { project: AccessProject; admin: boolean }) {
  const { t } = useTranslation();
  const load = useCallback(() => accessApi.repository(project.id), [project.id]);
  const repository = useAccessResource(load);
  const loadRemotes = useCallback(
    () => (admin ? accessApi.gitRemotes() : Promise.resolve([] as GitRemote[])),
    [admin]
  );
  const remotes = useAccessResource(loadRemotes, { headerRefresh: admin });
  const { run, busy } = useAccessActions(repository.refresh);
  const [draft, setDraft] = useState<string | null>(null);
  const current = repository.data ?? '';
  const selected = draft ?? current;

  return (
    <section className={styles.page}>
      <h2>{t('access.repository.title')}</h2>
      <p className={styles.sectionHint}>{t('access.repository.hint')}</p>
      <LoadState {...repository} retry={repository.refresh}>
        {admin ? (
          <LoadState {...remotes} retry={remotes.refresh}>
            <form
              className={styles.form}
              onSubmit={(event) => {
                event.preventDefault();
                if (selected === current || busy) return;
                void run(() => accessApi.setRepository(project.id, selected)).then((ok) => {
                  if (ok) setDraft(null);
                });
              }}
            >
              <Field
                label={t('access.repository.select')}
                hint={
                  remotes.data?.length
                    ? t('access.repository.select_hint')
                    : t('access.repository.no_remotes')
                }
              >
                {(id) => (
                  <Select
                    id={id}
                    value={selected}
                    fullWidth
                    disabled={busy}
                    options={repositoryOptions(remotes.data ?? [], current, {
                      none: t('access.repository.none_option'),
                      credentials: t('access.repository.credentials'),
                      public: t('access.repository.public'),
                      missing: t('access.repository.missing'),
                    })}
                    onChange={setDraft}
                  />
                )}
              </Field>
              <div className={styles.actions}>
                <Button type="submit" loading={busy} disabled={selected === current}>
                  {t('access.repository.save')}
                </Button>
              </div>
            </form>
          </LoadState>
        ) : (
          <>
            <dl className={styles.details}>
              <dt>{t('access.repository.current')}</dt>
              <dd className={current ? styles.mono : undefined}>
                {current || t('access.repository.not_set')}
              </dd>
            </dl>
            <p className={styles.fieldHint}>{t('access.repository.member_hint')}</p>
          </>
        )}
      </LoadState>
    </section>
  );
}
