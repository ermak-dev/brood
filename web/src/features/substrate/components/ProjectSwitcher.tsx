import { useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useLocation, useNavigate } from 'react-router-dom';
import { projectSelectionPath } from '@/features/access/shardState';
import { Button } from '@/components/ui/Button';
import { IconSidebarProjects } from '@/components/ui/icons';
import { useProjectStore } from '@/stores';
import { accessApi, type AccessProject } from '@/services/brood/access';
import { errorMessage } from '@/services/brood/http';

/** Глобальный контекст панели — проект, не рой. В меню только доступные
 * проекты; управление ими находится в левой панели, не дублируется здесь. */
export function ProjectSwitcher() {
  const { t } = useTranslation();
  const { pathname, search } = useLocation();
  const navigate = useNavigate();
  const project = useProjectStore((state) => state.project);
  const setProject = useProjectStore((state) => state.setProject);
  const [open, setOpen] = useState(false);
  const [projects, setProjects] = useState<AccessProject[] | null>(null);
  const [error, setError] = useState('');
  const menuRef = useRef<HTMLDivElement | null>(null);
  useEffect(() => {
    let current = true;
    setError('');
    accessApi
      .projects()
      .then((items) => {
        if (current) setProjects(items);
      })
      .catch((reason) => {
        if (current) setError(errorMessage(reason));
      });
    const onPointerDown = (event: MouseEvent) => {
      if (!menuRef.current?.contains(event.target as Node)) setOpen(false);
    };
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') setOpen(false);
    };
    document.addEventListener('mousedown', onPointerDown);
    document.addEventListener('keydown', onKeyDown);
    return () => {
      current = false;
      document.removeEventListener('mousedown', onPointerDown);
      document.removeEventListener('keydown', onKeyDown);
    };
  }, [open, project]);
  const current = projects?.find((item) => item.id === project)?.name;
  return (
    <div className={`language-menu project-switcher ${open ? 'open' : ''}`} ref={menuRef}>
      {/* Имя кнопки содержит видимый текст («Project: brood»): имя без него
       * скринридер и голосовое управление считают несовпадением (#256). */}
      <Button
        variant="ghost"
        size="sm"
        onClick={() => setOpen((previous) => !previous)}
        title={t('access.select_project')}
        aria-label={
          current ? t('access.current_project', { name: current }) : t('access.select_project')
        }
        aria-haspopup="menu"
        aria-expanded={open}
      >
        <IconSidebarProjects size={16} />
        <span className="project-switcher-label">{current || t('access.select_project')}</span>
      </Button>
      {open && (
        <div className="notification entering language-menu-popover" role="menu">
          {error ? (
            <div className="language-menu-option" role="alert">
              {error}
            </div>
          ) : (
            <>
              {projects === null && (
                <div className="language-menu-option">{t('common.loading')}</div>
              )}
              {projects?.length === 0 && (
                <div className="language-menu-option">{t('access.no_projects')}</div>
              )}
              {projects?.map((item) => (
                <button
                  key={item.id}
                  type="button"
                  className={`language-menu-option ${project === item.id ? 'active' : ''}`}
                  role="menuitemradio"
                  aria-checked={project === item.id}
                  onClick={() => {
                    setProject(item.id);
                    const destination = projectSelectionPath(pathname, item.id, search);
                    if (destination) navigate(destination);
                    setOpen(false);
                  }}
                >
                  <span>{item.name}</span>
                  {project === item.id && <span className="language-menu-check">✓</span>}
                </button>
              ))}
            </>
          )}
        </div>
      )}
    </div>
  );
}
