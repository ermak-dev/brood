import { useCallback, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { EmptyState } from '@/components/ui/EmptyState';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';
import { templatesApi, type ActorTemplate } from '@/services/brood/substrate';
import { EditorSheet } from './components/EditorSheet';
import { EntityTable, type Column } from './components/EntityTable';
import { Section } from './components/form';
import { PageHeader } from './components/PageHeader';
import { ProjectScope } from './components/ProjectScope';
import { DASH, formatCpu, formatDate, formatMemory, selectorText } from './format';
import { useActions, useResource } from './hooks';
import { TemplateEditor } from './TemplateEditor';
import styles from './substrate.module.scss';

/** brood: actor templates of the active project. ateapi keeps templates
 * immutable, so a change is a new template: "Duplicate" opens the editor on a
 * copy. */
export function TemplatesPage() {
  const { t } = useTranslation();
  return (
    <ProjectScope title={t('templates.title')}>
      {(project) => <Templates project={project} />}
    </ProjectScope>
  );
}

const fidelityOf = (tpl: ActorTemplate) =>
  (tpl.snapshotConfig?.preferredFidelity ?? '').replace('SNAPSHOT_FIDELITY_', '').toLowerCase();

const imagesOf = (tpl: ActorTemplate) => tpl.containers.map((c) => c.image).filter(Boolean);

function Templates({ project }: { project: string }) {
  const { t, i18n } = useTranslation();
  const load = useCallback(() => templatesApi.list(project), [project]);
  const { data, error, loading, refresh } = useResource(load);
  const { run, confirm } = useActions(refresh);
  const [viewing, setViewing] = useState<ActorTemplate | null>(null);
  const [editor, setEditor] = useState({ source: null as ActorTemplate | null, open: false, session: 0 });

  const openEditor = (source: ActorTemplate | null) => {
    setViewing(null);
    setEditor((prev) => ({ source, open: true, session: prev.session + 1 }));
  };
  const closeEditor = () => setEditor((prev) => ({ ...prev, open: false }));

  const remove = (tpl: ActorTemplate) =>
    confirm({
      title: t('templates.delete_title'),
      message: t('templates.delete_message', { name: tpl.metadata.name }),
      confirmText: t('substrate.delete'),
      success: t('templates.deleted', { name: tpl.metadata.name }),
      run: () => templatesApi.remove(project, tpl.metadata.name),
    });

  const goldenText = (tpl: ActorTemplate) => {
    const golden = tpl.status?.goldenSnapshotStatus;
    if (tpl.snapshotConfig?.goldenSnapshotConfig?.mode === 'GOLDEN_SNAPSHOT_MODE_DISABLED') {
      return t('templates.golden_disabled');
    }
    if (golden?.errorMessage) return t('templates.golden_failed', { error: golden.errorMessage });
    if (golden?.goldenTag) {
      return t('templates.golden_ready', {
        tag: golden.goldenTag.name,
        time: formatDate(golden.snapshotTime, i18n.language),
      });
    }
    return t('templates.golden_pending');
  };

  const fidelityLabel = (tpl: ActorTemplate) =>
    fidelityOf(tpl) ? t(`templates.fidelity_${fidelityOf(tpl)}`) : DASH;

  const columns: Column<ActorTemplate>[] = [
    { key: 'name', label: t('substrate.name'), primary: true, render: (tpl) => tpl.metadata.name },
    {
      key: 'image',
      label: t('templates.image'),
      mono: true,
      render: (tpl) => {
        const images = imagesOf(tpl);
        return images.length ? `${images[0]}${images.length > 1 ? ` +${images.length - 1}` : ''}` : DASH;
      },
    },
    {
      key: 'resources',
      label: t('templates.resources'),
      render: (tpl) =>
        `${formatCpu(tpl.resources?.limits?.cpuMillicores)} · ${formatMemory(tpl.resources?.limits?.memoryBytes)}`,
    },
    { key: 'fidelity', label: t('templates.fidelity'), hideOnMobile: true, render: fidelityLabel },
    {
      key: 'created',
      label: t('substrate.created'),
      hideOnMobile: true,
      render: (tpl) => formatDate(tpl.metadata.createTime, i18n.language),
    },
  ];

  const create = <Button onClick={() => openEditor(null)}>{t('templates.create')}</Button>;

  return (
    <div className={styles.page}>
      <PageHeader
        title={t('templates.title')}
        meta={[project, data && t('templates.meta_count', { count: data.length })]}
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
          rowKey={(tpl) => tpl.metadata.uid ?? tpl.metadata.name}
          onRowClick={setViewing}
          actionsLabel={t('substrate.actions')}
          actions={(tpl) => (
            <>
              <Button variant="ghost" size="sm" onClick={() => openEditor(tpl)}>
                {t('templates.duplicate')}
              </Button>
              <Button variant="ghost" size="sm" className={styles.dangerText} onClick={() => remove(tpl)}>
                {t('substrate.delete')}
              </Button>
            </>
          )}
        />
      ) : (
        <div className={styles.empty}>
          <EmptyState title={t('templates.empty')} description={t('templates.empty_hint')} action={create} />
        </div>
      )}

      <EditorSheet
        open={viewing !== null}
        onClose={() => setViewing(null)}
        size="lg"
        eyebrow={project}
        title={viewing?.metadata.name ?? ''}
        description={t('templates.immutable_hint')}
        extra={
          viewing && (
            <Button variant="secondary" onClick={() => openEditor(viewing)}>
              {t('templates.duplicate')}
            </Button>
          )
        }
      >
        {viewing && (
          <div className={styles.form}>
            <Section title={t('templates.summary')}>
              <dl className={styles.details}>
                {viewing.containers.map((c) => (
                  <ContainerSummary key={c.name} name={c.name} image={c.image} />
                ))}
                <dt>{t('templates.resources')}</dt>
                <dd>
                  {formatCpu(viewing.resources?.limits?.cpuMillicores)} ·{' '}
                  {formatMemory(viewing.resources?.limits?.memoryBytes)}
                </dd>
                <dt>{t('templates.fidelity')}</dt>
                <dd>{fidelityLabel(viewing)}</dd>
                <dt>{t('templates.golden')}</dt>
                <dd>{goldenText(viewing)}</dd>
                <dt>{t('templates.storage')}</dt>
                <dd className={styles.mono}>{viewing.snapshotConfig?.storageLocation || DASH}</dd>
                <dt>{t('substrate.placement')}</dt>
                <dd className={styles.mono}>{selectorText(viewing.workerSelector)}</dd>
                <dt>{t('substrate.created')}</dt>
                <dd>{formatDate(viewing.metadata.createTime, i18n.language)}</dd>
              </dl>
            </Section>
            <Section title="JSON" hint={t('templates.json_view_hint')}>
              <pre className={styles.code}>{JSON.stringify(viewing, null, 2)}</pre>
            </Section>
          </div>
        )}
      </EditorSheet>

      <TemplateEditor
        key={editor.session}
        project={project}
        source={editor.source}
        open={editor.open}
        onClose={closeEditor}
        onSubmit={async (tpl) => {
          const name = tpl.metadata.name;
          const ok = await run(() => templatesApi.create(project, tpl), t('templates.created', { name }));
          if (ok) closeEditor();
          return ok;
        }}
      />
    </div>
  );
}

function ContainerSummary({ name, image }: { name: string; image: string }) {
  return (
    <>
      <dt>{name}</dt>
      <dd className={styles.mono}>{image || DASH}</dd>
    </>
  );
}
