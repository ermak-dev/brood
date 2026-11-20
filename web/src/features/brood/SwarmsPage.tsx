import { useState } from 'react';
import { Link, useSearchParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { Input } from '@/components/ui/Input';
import { Select, type SelectOption } from '@/components/ui/Select';
import { accessApi, type Swarm } from '@/services/brood/access';
import { useResource } from './hooks';
import { agentsOf, swarmsOf } from './routes';
import { Field, FormDialog } from './table/dialogs';
import { useDialog, useForm } from './table/formState';
import { RelativeTime } from './table/format';
import { TablePage, type Column } from './table/TablePage';
import styles from './table/table.module.scss';

const SORT = { column: 'name', desc: false };

function CreateSwarm({
  open,
  close,
  after,
  projects,
  initialProject,
}: {
  open: boolean;
  close: () => void;
  after: () => unknown;
  projects: SelectOption[];
  initialProject: string;
}) {
  const { t } = useTranslation();
  const [chosen, setChosen] = useState(initialProject);
  const project = projects.some((option) => option.value === chosen)
    ? chosen
    : (projects[0]?.value ?? '');
  const form = useForm({ name: '' }, ({ name }) => ({
    name: name.trim() ? undefined : t('brood.form.required'),
  }));
  const name = form.values.name.trim();
  return (
    <FormDialog
      open={open}
      close={close}
      title={t('brood.swarms.create')}
      submitLabel={t('brood.form.create')}
      valid={form.valid && project !== ''}
      submit={() => accessApi.createSwarm(project, name)}
      success={t('brood.swarms.created', { name })}
      after={after}
    >
      <Input label={t('brood.name')} required autoComplete="off" {...form.field('name')} />
      <Field
        label={t('brood.swarms.project')}
        required
        error={projects.length === 0 ? t('brood.swarms.no_projects') : undefined}
      >
        {(id) => (
          <Select
            id={id}
            value={project}
            options={projects}
            disabled={projects.length === 0}
            onChange={setChosen}
          />
        )}
      </Field>
    </FormDialog>
  );
}

export function SwarmsPage() {
  const { t } = useTranslation();
  const [params] = useSearchParams();
  const swarms = useResource(accessApi.swarms);
  const projects = useResource(accessApi.projects);
  const create = useDialog();
  const projectOptions = (projects.data ?? []).map(({ id, name }) => ({ value: id, label: name }));

  const columns: Column<Swarm>[] = [
    {
      id: 'name',
      header: t('brood.name'),
      cell: (row) => (
        <Link className={styles.name} to={agentsOf(row.id)}>
          {row.name}
        </Link>
      ),
      text: (row) => row.name,
      sort: (row) => row.name,
    },
    {
      id: 'project',
      header: t('brood.swarms.project'),
      cell: (row) => <Link to={swarmsOf(row.project_id)}>{row.project_name}</Link>,
      text: (row) => row.project_name,
      sort: (row) => row.project_name,
    },
    {
      id: 'owner',
      header: t('brood.swarms.owner'),
      cell: (row) => row.owner_username,
      text: (row) => row.owner_username,
      sort: (row) => row.owner_username,
    },
    {
      id: 'agents',
      header: t('brood.agents.title'),
      numeric: true,
      cell: (row) => <Link to={agentsOf(row.id)}>{row.agentCount}</Link>,
      sort: (row) => row.agentCount,
    },
    {
      id: 'created',
      header: t('brood.swarms.created_at'),
      cell: (row) => <RelativeTime value={row.created_at} />,
      sort: (row) => row.created_at,
    },
  ];

  return (
    <TablePage
      title={t('brood.swarms.title')}
      description={t('brood.swarms.description')}
      primary={{ label: t('brood.swarms.create'), onClick: () => create.show(null) }}
      resource={swarms}
      rowKey={(row) => row.id}
      rowName={(row) => row.name}
      columns={columns}
      filters={[
        {
          param: 'project',
          label: t('brood.swarms.project'),
          all: t('brood.swarms.all_projects'),
          options: projectOptions,
          value: (row) => row.project_id,
        },
      ]}
      defaultSort={SORT}
      empty={t('brood.swarms.empty')}
      actions={(row) => [
        {
          id: 'delete',
          label: t('common.delete'),
          danger: true,
          confirm: t('brood.swarms.confirm_delete', { name: row.name }),
          run: () => accessApi.deleteSwarm(row.id),
        },
      ]}
    >
      {create.mount((dialog) => (
        <CreateSwarm
          {...dialog}
          after={swarms.refresh}
          projects={projectOptions}
          initialProject={params.get('project') ?? ''}
        />
      ))}
    </TablePage>
  );
}
