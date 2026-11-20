import { useCallback, useState } from 'react';
import { useParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { EmptyState } from '@/components/ui/EmptyState';
import { Input } from '@/components/ui/Input';
import { Select } from '@/components/ui/Select';
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/Table';
import { accessApi } from '@/services/brood/access';
import {
  workersApi,
  type Worker,
  type WorkerAction,
  type WorkerRole,
} from '@/services/brood/workers';
import { useActions, useAdministrator, useResource } from './hooks';
import { projectPath } from './routes';
import { DASH, Field, FormModal, LoadState, PageHeader, StatusBadge, type BadgeTone } from './ui';
import styles from './brood.module.scss';

/** How often the worker list is read while the tab is visible. */
const POLL_MS = 4000;

const PHASES = ['provisioning', 'running', 'stopping', 'stopped', 'error', 'deleting'];

const phaseTone = (phase: string): BadgeTone =>
  phase === 'error'
    ? 'problem'
    : phase === 'provisioning' || phase === 'stopping' || phase === 'deleting'
      ? 'progress'
      : 'neutral';

/** What the phase leaves to offer; the server decides for real. */
const offers = (worker: Worker, action: WorkerAction | 'delete' | 'terminal'): boolean => {
  switch (action) {
    case 'resume':
      return worker.phase === 'stopped' || worker.phase === 'error';
    case 'stop':
      return worker.phase === 'running' || worker.phase === 'provisioning';
    case 'terminal':
      return worker.phase === 'running';
    default:
      return worker.phase !== 'deleting';
  }
};

function CreateWorker({
  open,
  swarm,
  run,
  close,
}: {
  open: boolean;
  swarm: string;
  run: (action: () => Promise<unknown>) => Promise<boolean>;
  close: () => void;
}) {
  const { t } = useTranslation();
  const administrator = useAdministrator();
  const profiles = useResource(useCallback(() => workersApi.profiles(swarm), [swarm]));
  const [name, setName] = useState('');
  const [chosen, setChosen] = useState('');
  const [role, setRole] = useState<WorkerRole>('worker');
  const [origin, setOrigin] = useState('');
  const options = (profiles.data ?? []).map((profile) => ({ value: profile, label: profile }));
  const profile = options.some((option) => option.value === chosen)
    ? chosen
    : (options[0]?.value ?? '');
  return (
    <FormModal
      open={open}
      title={t('brood.workers.create')}
      submitLabel={t('brood.create')}
      canSubmit={name.trim() !== '' && profile !== ''}
      submit={() =>
        run(() =>
          workersApi.create(swarm, { name: name.trim(), profile, role, origin: origin.trim() })
        )
      }
      close={close}
    >
      <Input
        label={t('brood.name')}
        value={name}
        autoFocus
        autoComplete="off"
        autoCapitalize="off"
        spellCheck={false}
        onChange={(event) => setName(event.target.value)}
      />
      <Field label={t('brood.workers.profile')}>
        {(id) => (
          <LoadState {...profiles} ready={profiles.data !== null} retry={profiles.refresh}>
            {options.length === 0 ? (
              <p className={styles.fieldHint}>{t('brood.workers.no_profiles')}</p>
            ) : (
              <Select id={id} fullWidth value={profile} options={options} onChange={setChosen} />
            )}
          </LoadState>
        )}
      </Field>
      {administrator && (
        <Field label={t('brood.workers.role')} hint={t('brood.workers.role_hint')}>
          {(id) => (
            <Select
              id={id}
              fullWidth
              value={role}
              options={[
                { value: 'worker', label: t('brood.workers.roles.worker') },
                { value: 'master', label: t('brood.workers.roles.master') },
              ]}
              onChange={(value) => setRole(value === 'master' ? 'master' : 'worker')}
            />
          )}
        </Field>
      )}
      <Input
        label={t('brood.workers.origin')}
        hint={t('brood.workers.origin_hint')}
        value={origin}
        autoComplete="off"
        autoCapitalize="off"
        spellCheck={false}
        onChange={(event) => setOrigin(event.target.value)}
      />
    </FormModal>
  );
}

export function SwarmPage() {
  const { t } = useTranslation();
  const { id = '' } = useParams();
  const swarm = useResource(useCallback(() => accessApi.swarm(id), [id]));
  const projects = useResource(accessApi.projects);
  const workers = useResource(
    useCallback(() => workersApi.list(id), [id]),
    swarm.data ? POLL_MS : null
  );
  const { run, confirm, busy } = useActions(workers.refresh);
  // Each opening is a new dialog (session): fresh fields and a fresh profile list.
  const [dialog, setDialog] = useState({ open: false, session: 0 });
  const project = projects.data?.find((row) => row.id === swarm.data?.project_id);
  const rows = workers.data ?? [];
  const phaseLabel = (phase: string) =>
    PHASES.includes(phase) ? t(`brood.workers.phases.${phase}`) : phase || DASH;
  const act = (worker: Worker, action: WorkerAction) => () =>
    workersApi.act(id, worker.name, action);
  const button = (worker: Worker, action: WorkerAction | 'delete', onClick: () => void) => (
    <Button
      variant={action === 'resume' || action === 'stop' ? 'secondary' : 'ghost'}
      size="sm"
      className={action === 'reset' || action === 'delete' ? styles.dangerText : undefined}
      disabled={busy || !offers(worker, action)}
      aria-label={t(`brood.workers.${action}_named`, { name: worker.name })}
      onClick={onClick}
    >
      {t(`brood.workers.${action}`)}
    </Button>
  );
  return (
    <div className={styles.page}>
      <PageHeader
        title={swarm.data?.name ?? t('brood.swarm.title')}
        crumbs={[
          { to: '/projects', label: t('brood.projects.title') },
          ...(swarm.data
            ? [
                {
                  to: projectPath(swarm.data.project_id),
                  label: project?.name ?? t('brood.project.title'),
                },
              ]
            : []),
        ]}
        meta={swarm.data ? [t('brood.swarm.owner', { name: swarm.data.owner_username })] : []}
        actions={
          swarm.data && (
            <Button onClick={() => setDialog({ open: true, session: dialog.session + 1 })}>
              {t('brood.workers.create')}
            </Button>
          )
        }
      />
      <LoadState {...swarm} ready={swarm.data !== null} retry={swarm.refresh}>
        <LoadState {...workers} ready={workers.data !== null} retry={workers.refresh}>
          {rows.length === 0 ? (
            <EmptyState
              title={t('brood.workers.empty')}
              description={t('brood.workers.empty_hint')}
            />
          ) : (
            <Table>
              <TableHeader>
                <tr>
                  <TableHead>{t('brood.name')}</TableHead>
                  <TableHead>{t('brood.workers.role')}</TableHead>
                  <TableHead>{t('brood.workers.state')}</TableHead>
                  <TableHead>{t('brood.workers.placement')}</TableHead>
                  <TableHead />
                </tr>
              </TableHeader>
              <TableBody>
                {rows.map((worker) => (
                  <TableRow key={worker.name}>
                    <TableCell>
                      <span className={styles.nameLink}>{worker.name}</span>
                    </TableCell>
                    <TableCell>
                      {worker.role === 'worker' || worker.role === 'master'
                        ? t(`brood.workers.roles.${worker.role}`)
                        : worker.role || DASH}
                    </TableCell>
                    <TableCell>
                      <StatusBadge tone={phaseTone(worker.phase)}>
                        {phaseLabel(worker.phase)}
                      </StatusBadge>
                      {worker.error && <p className={styles.errorText}>{worker.error}</p>}
                    </TableCell>
                    <TableCell className={styles.mono}>
                      {worker.pool || DASH}
                      {worker.ctid !== null && ` · CT ${worker.ctid}`}
                    </TableCell>
                    <TableCell>
                      <div className={styles.rowActions}>
                        {offers(worker, 'terminal') ? (
                          <a
                            className="btn btn-secondary btn-sm"
                            href={workersApi.terminal(id, worker.name)}
                            target="_blank"
                            rel="noopener noreferrer"
                            aria-label={t('brood.workers.terminal_named', { name: worker.name })}
                          >
                            <span>{t('brood.workers.terminal')}</span>
                          </a>
                        ) : (
                          <Button
                            variant="secondary"
                            size="sm"
                            disabled
                            title={t('brood.workers.terminal_not_running')}
                          >
                            {t('brood.workers.terminal')}
                          </Button>
                        )}
                        {button(worker, 'resume', () => void run(act(worker, 'resume')))}
                        {button(worker, 'stop', () => void run(act(worker, 'stop')))}
                        {button(worker, 'reset', () =>
                          confirm({
                            title: t('brood.workers.reset'),
                            message: t('brood.workers.confirm_reset', { name: worker.name }),
                            action: act(worker, 'reset'),
                          })
                        )}
                        {button(worker, 'delete', () =>
                          confirm({
                            title: t('brood.workers.delete'),
                            message: t('brood.workers.confirm_delete', { name: worker.name }),
                            action: () => workersApi.remove(id, worker.name),
                          })
                        )}
                      </div>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </LoadState>
      </LoadState>
      {dialog.session > 0 && (
        <CreateWorker
          key={dialog.session}
          open={dialog.open}
          swarm={id}
          run={run}
          close={() => setDialog({ ...dialog, open: false })}
        />
      )}
    </div>
  );
}
