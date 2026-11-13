import { useTranslation } from 'react-i18next';
import { useAdministrator } from '@/features/access/shared';
import { PageHeader } from '@/features/substrate/components/PageHeader';
import { useAuthStore } from '@/stores';
import { TelegramSection } from './TelegramSection';
import styles from '@/features/substrate/substrate.module.scss';

/** Профиль вошедшего пользователя (#285): кто вошёл и его секции. Всё на
 * странице -- о самом пользователе; права других сервер не показывает. */
export function ProfilePage() {
  const { t } = useTranslation();
  const username = useAuthStore((state) => state.username);
  const administrator = useAdministrator();
  return (
    <div className={styles.page}>
      <PageHeader
        title={t('profile.title')}
        meta={[username, t(administrator ? 'profile.role_admin' : 'profile.role_member')]}
      />
      <TelegramSection />
    </div>
  );
}
