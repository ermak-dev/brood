import { useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { EmptyState } from '@/components/ui/EmptyState';
import { Input } from '@/components/ui/Input';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';
import { useProjectStore } from '@/stores';
import { projectsApi, type Atespace } from '@/services/brood/substrate';
import { EditorSheet } from './components/EditorSheet';
import { EntityTable, type Column } from './components/EntityTable';
import { PageHeader } from './components/PageHeader';
import { formatDate, isShortName } from './format';
import { useActions, useResource } from './hooks';
import styles from './substrate.module.scss';

/** brood: projects -- the install's atespaces. An atespace has no attributes
 * besides its name, so the editor only creates; clicking a row makes the
 * project active for the actors and templates pages. */
export function ProjectsPage() {
  const { t, i18n } = useTranslation();
  const navigate = useNavigate();
  const active = useProjectStore((state) => state.project);
  const setActive = useProjectStore((state) => state.setProject);
  const { data, error, loading, refresh } = useResource(projectsApi.list);
  const { run, confirm } = useActions(refresh);
  const [creating, setCreating] = useState(false);

  const openIn = (name: string, to: string) => {
    setActive(name);
    navigate(to);
  };

  const remove = (name: string) =>
    confirm({
      title: t('projects.delete_title'),
      message: t('projects.delete_message', { name }),
      confirmText: t('substrate.delete'),
      success: t('projects.deleted', { name }),
      run: async () => {
        await projectsApi.remove(name);
        if (active === name) setActive('');
      },
    });

  const columns: Column<Atespace>[] = [
    {
      key: 'name',
      label: t('substrate.name'),
      primary: true,
      render: (p) => (
        <>
          {p.metadata.name}
          {p.metadata.name === active && (
            <span className={`${styles.badge} ${styles['tone-success']} ${styles.activeMark}`}>
              {t('projects.active')}
            </span>
          )}
        </>
      ),
    },
    {
      key: 'created',
      label: t('substrate.created'),
      render: (p) => formatDate(p.metadata.createTime, i18n.language),
    },
    { key: 'uid', label: 'UID', mono: true, hideOnMobile: true, render: (p) => p.metadata.uid },
  ];

  const create = (
    <Button onClick={() => setCreating(true)}>{t('projects.create')}</Button>
  );

  return (
    <div className={styles.page}>
      <PageHeader
        title={t('projects.title')}
        meta={[
          data && t('projects.meta_count', { count: data.length }),
          active ? t('projects.meta_active', { name: active }) : t('projects.meta_none'),
        ]}
        actions={create}
      />

      {loading ? (
        <LoadingSpinner />
      ) : error ? (
        <div className={styles.empty}>
          <EmptyState title={t('substrate.load_failed')} description={error} />
        </div>
      ) : data && data.length > 0 ? (
        <EntityTable
          columns={columns}
          rows={data}
          rowKey={(p) => p.metadata.name}
          isActive={(p) => p.metadata.name === active}
          onRowClick={(p) => setActive(p.metadata.name)}
          actionsLabel={t('substrate.actions')}
          actions={(p) => (
            <>
              <Button variant="ghost" size="sm" onClick={() => openIn(p.metadata.name, '/actors')}>
                {t('nav.actors')}
              </Button>
              <Button variant="ghost" size="sm" onClick={() => openIn(p.metadata.name, '/templates')}>
                {t('nav.templates')}
              </Button>
              <Button
                variant="ghost"
                size="sm"
                className={styles.dangerText}
                onClick={() => remove(p.metadata.name)}
              >
                {t('substrate.delete')}
              </Button>
            </>
          )}
        />
      ) : (
        <div className={styles.empty}>
          <EmptyState title={t('projects.empty')} description={t('projects.empty_hint')} action={create} />
        </div>
      )}

      <ProjectEditor
        open={creating}
        onClose={() => setCreating(false)}
        onSubmit={async (name) => {
          const ok = await run(() => projectsApi.create(name), t('projects.created', { name }));
          if (ok) {
            setActive(name);
            setCreating(false);
          }
          return ok;
        }}
      />
    </div>
  );
}

interface ProjectEditorProps {
  open: boolean;
  onClose: () => void;
  /** Resolves true once created; on a refusal the name stays for fixing. */
  onSubmit: (name: string) => Promise<boolean>;
}

function ProjectEditor({ open, onClose, onSubmit }: ProjectEditorProps) {
  const { t } = useTranslation();
  const [name, setName] = useState('');
  const [saving, setSaving] = useState(false);
  const valid = isShortName(name);

  const close = () => {
    setName('');
    onClose();
  };

  const save = async () => {
    if (!valid) return;
    setSaving(true);
    try {
      if (await onSubmit(name)) setName('');
    } finally {
      setSaving(false);
    }
  };

  return (
    <EditorSheet
      open={open}
      onClose={close}
      title={t('projects.create_title')}
      description={t('projects.create_hint')}
      onSave={() => void save()}
      saveLabel={t('substrate.create')}
      canSave={valid}
      saving={saving}
      dirty={name !== ''}
    >
      <form
        className={styles.form}
        onSubmit={(event) => {
          event.preventDefault();
          void save();
        }}
      >
        <Input
          name="project-name"
          label={t('substrate.name')}
          value={name}
          autoFocus
          autoComplete="off"
          placeholder="brood"
          hint={t('substrate.name_hint')}
          error={name && !valid ? t('substrate.name_invalid') : undefined}
          onChange={(event) => setName(event.target.value.trim())}
        />
      </form>
    </EditorSheet>
  );
}
