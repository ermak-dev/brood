import { useState } from 'react';
import { Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
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
import { accessApi } from '@/services/brood/access';
import { useActions, useAdministrator, useResource } from './hooks';
import { projectPath } from './routes';
import { FormModal, LoadState, PageHeader } from './ui';
import styles from './brood.module.scss';

export function ProjectsPage() {
  const { t } = useTranslation();
  const administrator = useAdministrator();
  const projects = useResource(accessApi.projects);
  const { run, confirm, busy } = useActions(projects.refresh);
  const [creating, setCreating] = useState(false);
  const [name, setName] = useState('');
  const rows = projects.data ?? [];
  return (
    <div className={styles.page}>
      <PageHeader
        title={t('brood.projects.title')}
        actions={
          administrator && (
            <Button
              onClick={() => {
                setName('');
                setCreating(true);
              }}
            >
              {t('brood.projects.create')}
            </Button>
          )
        }
      />
      <LoadState {...projects} ready={projects.data !== null} retry={projects.refresh}>
        {rows.length === 0 ? (
          <EmptyState
            title={t('brood.projects.empty')}
            description={administrator ? undefined : t('brood.projects.empty_member')}
          />
        ) : (
          <Table>
            <TableHeader>
              <tr>
                <TableHead>{t('brood.name')}</TableHead>
                <TableHead alignRight>{t('brood.projects.swarms')}</TableHead>
                {administrator && <TableHead />}
              </tr>
            </TableHeader>
            <TableBody>
              {rows.map((project) => (
                <TableRow key={project.id}>
                  <TableCell>
                    <Link className={styles.nameLink} to={projectPath(project.id)}>
                      {project.name}
                    </Link>
                  </TableCell>
                  <TableCell alignRight>{project.swarmCount ?? 0}</TableCell>
                  {administrator && (
                    <TableCell>
                      <div className={styles.rowActions}>
                        <Button
                          variant="ghost"
                          size="sm"
                          className={styles.dangerText}
                          disabled={busy}
                          aria-label={t('brood.delete_named', { name: project.name })}
                          onClick={() =>
                            confirm({
                              title: t('common.delete'),
                              message: t('brood.projects.confirm_delete', { name: project.name }),
                              action: () => accessApi.deleteProject(project.id),
                            })
                          }
                        >
                          {t('common.delete')}
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
        <FormModal
          open={creating}
          title={t('brood.projects.create')}
          submitLabel={t('brood.create')}
          canSubmit={name.trim() !== ''}
          submit={() => run(() => accessApi.createProject(name.trim()))}
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
      )}
    </div>
  );
}
