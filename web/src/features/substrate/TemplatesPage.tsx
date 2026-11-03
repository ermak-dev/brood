import { useCallback, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { EmptyState } from '@/components/ui/EmptyState';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';
import { templatesApi, type ActorTemplate } from '@/services/brood/substrate';
import { swarmCapabilities } from '@/features/access/shardState';
import { useAdministrator } from '@/features/access/shared';
import { EntityPanel } from './components/EntityPanel';
import { EntityTable, type Column } from './components/EntityTable';
import { Section } from './components/form';
import { PageHeader } from './components/PageHeader';
import { ProjectScope } from './components/ProjectScope';
import { SwarmSections } from './components/SwarmSections';
import { DASH, formatCpu, formatDate, formatMemory, selectorText } from './format';
import { useActions, useResource } from './hooks';
import { TemplateEditor } from './TemplateEditor';
import { templateActionGroups } from './templateActions';
import { ManagedProfile } from './components/ManagedProfile';
import styles from './substrate.module.scss';

/** brood: actor templates of the active project. ateapi keeps templates
 * immutable, so a change is a new template: "Duplicate" opens the editor on a
 * copy. A row opens the template's side panel, where its actions live (#157). */
export function TemplatesPage() {
  const { t } = useTranslation();
  const admin = useAdministrator();
  return (
    <ProjectScope title={t('templates.title')}>
      {(project) => (
        <SwarmSections project={project} title={t('templates.title')}>
          {(swarm) => (
            <Templates
              project={swarm.id}
              name={swarm.name}
              canManage={swarmCapabilities(swarm, admin).manage}
            />
          )}
        </SwarmSections>
      )}
    </ProjectScope>
  );
}

const fidelityOf = (tpl: ActorTemplate) =>
  (tpl.snapshotConfig?.preferredFidelity ?? '').replace('SNAPSHOT_FIDELITY_', '').toLowerCase();

const imagesOf = (tpl: ActorTemplate) => (tpl.containers ?? []).map((c) => c.image).filter(Boolean);

const keyOf = (tpl: ActorTemplate) => tpl.metadata.uid ?? tpl.metadata.name;

function Templates({
  project,
  name,
  canManage,
}: {
  project: string;
  name: string;
  canManage: boolean;
}) {
  const { t, i18n } = useTranslation();
  const load = useCallback(() => templatesApi.list(project), [project]);
  const { data, error, loading, refresh } = useResource(load);
  const { run, confirm } = useActions(refresh);
  // Ключ, а не объект: после обновления списка панель показывает свежий
  // шаблон (статус золотого снапшота), а удалённый закрывает её сам.
  const [viewingKey, setViewingKey] = useState<string | null>(null);
  const viewing = (viewingKey && data?.find((tpl) => keyOf(tpl) === viewingKey)) || null;
  const [editor, setEditor] = useState({
    source: null as ActorTemplate | null,
    open: false,
    session: 0,
  });

  const openEditor = (source: ActorTemplate | null) => {
    setViewingKey(null);
    setEditor((prev) => ({ source, open: true, session: prev.session + 1 }));
  };
  const closeEditor = () => setEditor((prev) => ({ ...prev, open: false }));

  const goldenText = (tpl: ActorTemplate) => {
    if (tpl.provider === 'proxmox') return t('actors.suspend_unsupported');
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
        if (tpl.provider === 'proxmox') return `Proxmox · ${tpl.profile ?? DASH}`;
        const images = imagesOf(tpl);
        return images.length
          ? `${images[0]}${images.length > 1 ? ` +${images.length - 1}` : ''}`
          : DASH;
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

  const create = canManage ? (
    <Button onClick={() => openEditor(null)}>{t('templates.create')}</Button>
  ) : undefined;

  return (
    <div className={styles.page}>
      <PageHeader
        title={name}
        headingLevel={2}
        meta={[data && t('templates.meta_count', { count: data.length })]}
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
          rowKey={keyOf}
          onRowClick={(tpl) => setViewingKey(keyOf(tpl))}
          isActive={(tpl) => keyOf(tpl) === viewingKey}
        />
      ) : (
        <div className={styles.empty}>
          <EmptyState
            title={t('templates.empty')}
            description={t('templates.empty_hint')}
            action={create}
          />
        </div>
      )}

      <EntityPanel
        open={viewing !== null}
        onClose={() => setViewingKey(null)}
        eyebrow={project}
        title={viewing?.metadata.name ?? ''}
        description={t('templates.immutable_hint')}
        actions={
          viewing
            ? templateActionGroups(viewing, {
                t,
                canManage,
                confirm,
                duplicate: () => openEditor(viewing),
                remove: async () => {
                  await templatesApi.remove(project, viewing.metadata.name);
                  setViewingKey(null);
                },
              })
            : []
        }
      >
        {viewing && (
          <div className={styles.form}>
            <Section title={t('templates.summary')}>
              {viewing.provider === 'proxmox' && <ManagedProfile entity={viewing} />}
              <dl className={styles.details}>
                {(viewing.containers ?? []).map((c) => (
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
      </EntityPanel>

      {canManage && (
        <TemplateEditor
          key={editor.session}
          project={project}
          source={editor.source}
          open={editor.open}
          onClose={closeEditor}
          onSubmit={async (tpl) => {
            const name = tpl.metadata.name;
            const ok = await run(
              () => templatesApi.create(project, tpl),
              t('templates.created', { name })
            );
            if (ok) closeEditor();
            return ok;
          }}
        />
      )}
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
