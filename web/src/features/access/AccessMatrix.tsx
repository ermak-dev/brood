import { useTranslation } from 'react-i18next';
import { EntityTable } from '@/features/substrate/components/EntityTable';
import { Section } from '@/features/substrate/components/form';

const rules = [
  { resource: 'access.projects', member: 'access.matrix_member_projects' },
  { resource: 'access.swarms', member: 'access.matrix_own' },
  { resource: 'nav.templates', member: 'access.matrix_own' },
  { resource: 'nav.actors', member: 'access.matrix_own' },
  { resource: 'access.users', member: 'access.matrix_none' },
  { resource: 'access.infrastructure', member: 'access.matrix_none' },
  { resource: 'access.gateway', member: 'access.matrix_none' },
  { resource: 'access.administration', member: 'access.matrix_none' },
];

/** Матрица описывает действующие серверные правила, а не второй механизм
 * выдачи прав. Роль меняется в форме пользователя, членство — ниже. */
export function AccessMatrix() {
  const { t } = useTranslation();
  return (
    <Section title={t('access.matrix')} hint={t('access.server_authority')}>
      <EntityTable
        rows={rules}
        rowKey={(row) => row.resource}
        columns={[
          {
            key: 'resource',
            label: t('access.resource'),
            primary: true,
            render: (row) => t(row.resource),
          },
          { key: 'admin', label: t('access.administrator'), render: () => t('access.matrix_all') },
          { key: 'member', label: t('access.user'), render: (row) => t(row.member) },
        ]}
      />
    </Section>
  );
}
