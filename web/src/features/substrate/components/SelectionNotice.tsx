import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import styles from '../substrate.module.scss';

/** Адрес назвал то, чего страница показать не может (#194): объект удалён,
 * переименован, рой не активен или панель недоступна. Причина -- словами, а
 * действие снимает выбор из адреса; страница не подменяет его другим
 * объектом молча. */
export function SelectionNotice({
  text,
  action,
  onAction,
}: {
  text: string;
  action: string;
  onAction: () => void;
}) {
  return (
    <p role="status" className={styles.notice} data-selection-notice="">
      {text}{' '}
      <Button type="button" variant="ghost" size="sm" onClick={onAction}>
        {action}
      </Button>
    </p>
  );
}

/** Объект адреса, чья панель не открывается: его нет в списке или открыть
 * её нельзя. «Закрыть» снимает параметр объекта, как закрытие панели. */
export function ObjectUnavailable({ name, onClose }: { name: string; onClose: () => void }) {
  const { t } = useTranslation();
  return (
    <SelectionNotice
      text={t('substrate.object_unavailable', { name })}
      action={t('common.close')}
      onAction={onClose}
    />
  );
}
