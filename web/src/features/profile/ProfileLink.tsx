import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import { IconUser } from '@/components/ui/icons';
import { useAuthStore } from '@/stores';
import { PROFILE_PATH } from './routes';

/** Кнопка шапки к профилю: рядом с выходом, подпись называет вошедшего. */
export function ProfileLink() {
  const { t } = useTranslation();
  const username = useAuthStore((state) => state.username);
  const label = t('header.profile_of', { user: username });
  return (
    <Link className="btn btn-ghost btn-sm" to={PROFILE_PATH} title={label} aria-label={label}>
      <span>
        <IconUser size={16} />
      </span>
    </Link>
  );
}
