import { Fragment, useCallback, useState, type FormEvent } from 'react';
import { Link, useParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { Card } from '@/components/ui/Card';
import { EmptyState } from '@/components/ui/EmptyState';
import { Input } from '@/components/ui/Input';
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/Table';
import { accessApi, LIMIT_FIELDS, type ProjectLimits } from '@/services/brood/access';
import { useActions, useAdministrator, useResource } from './hooks';
import { swarmPath } from './routes';
import { DASH, FormModal, LoadState, PageHeader, Section } from './ui';
import styles from './brood.module.scss';

function Swarms({ project }: { project: string }) {
  const { t } = useTranslation();
  const swarms = useResource(useCallback(() => accessApi.swarms(project), [project]));
  const { run, confirm, busy } = useActions(swarms.refresh);
  const [creating, setCreating] = useState(false);
  const [name, setName] = useState('');
  const rows = swarms.data ?? [];
  return (
    <Card
      title={t('brood.swarms.title')}
      extra={
        <Button
          size="sm"
          onClick={() => {
            setName('');
            setCreating(true);
          }}
        >
          {t('brood.swarms.create')}
        </Button>
      }
    >
      <LoadState {...swarms} ready={swarms.data !== null} retry={swarms.refresh}>
        {rows.length === 0 ? (
          <EmptyState title={t('brood.swarms.empty')} />
        ) : (
          <Table>
            <TableHeader>
              <tr>
                <TableHead>{t('brood.name')}</TableHead>
                <TableHead>{t('brood.swarms.owner')}</TableHead>
                <TableHead alignRight>{t('brood.swarms.workers')}</TableHead>
                <TableHead />
              </tr>
            </TableHeader>
            <TableBody>
              {rows.map((swarm) => (
                <TableRow key={swarm.id}>
                  <TableCell>
                    <Link className={styles.nameLink} to={swarmPath(swarm.id)}>
                      {swarm.name}
                    </Link>
                  </TableCell>
                  <TableCell>{swarm.owner_username}</TableCell>
                  <TableCell alignRight>{swarm.managedActorCount ?? 0}</TableCell>
                  <TableCell>
                    <div className={styles.rowActions}>
                      <Button
                        variant="ghost"
                        size="sm"
                        className={styles.dangerText}
                        disabled={busy}
                        aria-label={t('brood.delete_named', { name: swarm.name })}
                        onClick={() =>
                          confirm({
                            title: t('common.delete'),
                            message: t('brood.swarms.confirm_delete', { name: swarm.name }),
                            action: () => accessApi.deleteSwarm(swarm.id),
                          })
                        }
                      >
                        {t('common.delete')}
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </LoadState>
      <FormModal
        open={creating}
        title={t('brood.swarms.create')}
        submitLabel={t('brood.create')}
        canSubmit={name.trim() !== ''}
        submit={() => run(() => accessApi.createSwarm(project, name.trim()))}
        close={() => setCreating(false)}
      >
        <Input
          label={t('brood.name')}
          value={name}
          autoFocus
          autoComplete="off"
          onChange={(event) => setName(event.target.value)}
        />
      </FormModal>
    </Card>
  );
}

function Members({ project }: { project: string }) {
  const { t } = useTranslation();
  const administrator = useAdministrator();
  const members = useResource(useCallback(() => accessApi.members(project), [project]));
  const { run, confirm, busy } = useActions(members.refresh);
  const [username, setUsername] = useState('');
  const rows = members.data ?? [];
  const add = async (event: FormEvent) => {
    event.preventDefault();
    const name = username.trim();
    if (name && (await run(() => accessApi.addMember(project, name)))) setUsername('');
  };
  return (
    <Card title={t('brood.members.title')}>
      <LoadState {...members} ready={members.data !== null} retry={members.refresh}>
        {rows.length === 0 ? (
          <EmptyState title={t('brood.members.empty')} />
        ) : (
          <Table>
            <TableHeader>
              <tr>
                <TableHead>{t('brood.users.username')}</TableHead>
                {administrator && <TableHead />}
              </tr>
            </TableHeader>
            <TableBody>
              {rows.map((member) => (
                <TableRow key={member.username}>
                  <TableCell>{member.username}</TableCell>
                  {administrator && (
                    <TableCell>
                      <div className={styles.rowActions}>
                        <Button
                          variant="ghost"
                          size="sm"
                          className={styles.dangerText}
                          disabled={busy}
                          aria-label={t('brood.members.remove_named', { name: member.username })}
                          onClick={() =>
                            confirm({
                              title: t('brood.members.remove'),
                              message: t('brood.members.confirm_remove', {
                                name: member.username,
                              }),
                              action: () => accessApi.removeMember(project, member.username),
                            })
                          }
                        >
                          {t('brood.members.remove')}
                        </Button>
                      </div>
                    </TableCell>
                  )}
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </LoadState>
      {administrator && (
        <form className={styles.inlineForm} onSubmit={(event) => void add(event)}>
          <Input
            label={t('brood.users.username')}
            value={username}
            autoComplete="off"
            autoCapitalize="off"
            spellCheck={false}
            onChange={(event) => setUsername(event.target.value)}
          />
          <Button type="submit" variant="secondary" disabled={busy || username.trim() === ''}>
            {t('brood.members.add')}
          </Button>
        </form>
      )}
    </Card>
  );
}

type LimitDraft = Record<keyof ProjectLimits, string>;

const toDraft = (limits: ProjectLimits): LimitDraft => ({
  max_swarms: limits.max_swarms ?? '',
  max_actors: limits.max_actors ?? '',
  max_cpu_millicores: limits.max_cpu_millicores ?? '',
  max_memory_bytes: limits.max_memory_bytes ?? '',
});

/** The server takes nonnegative decimal strings; empty means unlimited. */
const validLimit = (value: string) => /^(?:|0|[1-9][0-9]{0,17})$/.test(value.trim());

function Limits({ project }: { project: string }) {
  const { t } = useTranslation();
  const administrator = useAdministrator();
  const limits = useResource(useCallback(() => accessApi.limits(project), [project]));
  const { run, busy } = useActions(limits.refresh);
  const [edited, setEdited] = useState<LimitDraft | null>(null);
  if (limits.loading || !limits.data) {
    return (
      <LoadState {...limits} ready={false} retry={limits.refresh}>
        {null}
      </LoadState>
    );
  }
  const saved = toDraft(limits.data);
  const draft = edited ?? saved;
  const dirty = LIMIT_FIELDS.some((field) => draft[field].trim() !== saved[field]);
  const valid = LIMIT_FIELDS.every((field) => validLimit(draft[field]));
  if (!administrator) {
    return (
      <dl className={styles.details}>
        {LIMIT_FIELDS.map((field) => (
          <Fragment key={field}>
            <dt>{t(`brood.settings.${field}`)}</dt>
            <dd>{saved[field] || t('brood.settings.unlimited')}</dd>
          </Fragment>
        ))}
      </dl>
    );
  }
  const save = async (event: FormEvent) => {
    event.preventDefault();
    if (!dirty || !valid) return;
    const body = Object.fromEntries(
      LIMIT_FIELDS.map((field) => [field, draft[field].trim() || null])
    ) as unknown as ProjectLimits;
    if (await run(() => accessApi.setLimits(project, body))) setEdited(null);
  };
  return (
    <form className={styles.form} onSubmit={(event) => void save(event)}>
      <div className={styles.grid2}>
        {LIMIT_FIELDS.map((field) => (
          <Input
            key={field}
            label={t(`brood.settings.${field}`)}
            value={draft[field]}
            inputMode="numeric"
            autoComplete="off"
            placeholder={t('brood.settings.unlimited')}
            disabled={busy}
            error={validLimit(draft[field]) ? undefined : t('brood.settings.limit_invalid')}
            onChange={(event) => setEdited({ ...draft, [field]: event.target.value })}
          />
        ))}
      </div>
      <div className={styles.formActions}>
        <Button type="submit" size="sm" disabled={!dirty || !valid} loading={busy}>
          {t('common.save')}
        </Button>
      </div>
    </form>
  );
}

function Settings({ project }: { project: string }) {
  const { t } = useTranslation();
  const repository = useResource(useCallback(() => accessApi.repository(project), [project]));
  return (
    <Card title={t('brood.settings.title')}>
      <div className={styles.form}>
        <Section
          nested
          title={t('brood.settings.repository')}
          hint={t('brood.settings.repository_hint')}
        >
          <LoadState {...repository} ready={repository.data !== null} retry={repository.refresh}>
            <p className={styles.mono}>{repository.data || DASH}</p>
          </LoadState>
        </Section>
        <Section nested title={t('brood.settings.limits')} hint={t('brood.settings.limits_hint')}>
          <Limits project={project} />
        </Section>
      </div>
    </Card>
  );
}

export function ProjectPage() {
  const { t } = useTranslation();
  const { id = '' } = useParams();
  const projects = useResource(accessApi.projects);
  const project = projects.data?.find((row) => row.id === id);
  return (
    <div className={styles.page}>
      <PageHeader
        title={project?.name ?? t('brood.project.title')}
        crumbs={[{ to: '/projects', label: t('brood.projects.title') }]}
      />
      <LoadState {...projects} ready={projects.data !== null} retry={projects.refresh}>
        {project ? (
          <>
            <Swarms project={id} />
            <Members project={id} />
            <Settings project={id} />
          </>
        ) : (
          <EmptyState title={t('brood.project.not_found')} />
        )}
      </LoadState>
    </div>
  );
}
