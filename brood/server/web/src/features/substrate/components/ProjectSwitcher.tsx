import { useEffect, useRef, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { IconSidebarProjects } from '@/components/ui/icons';
import { useProjectStore } from '@/stores';
import { projectsApi } from '@/services/brood/substrate';

/** brood: the active project (atespace) switcher in the header, beside the
 * language menu and in its popover style. Actors and templates are scoped to
 * it. The list is fetched each time the menu opens, so a project created a
 * moment ago is there. */
export function ProjectSwitcher() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const project = useProjectStore((state) => state.project);
  const setProject = useProjectStore((state) => state.setProject);
  const [open, setOpen] = useState(false);
  const [names, setNames] = useState<string[] | null>(null);
  const menuRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (!open) return;
    let current = true;
    setNames(null);
    projectsApi
      .list()
      .then((projects) => current && setNames(projects.map((p) => p.metadata.name)))
      .catch(() => current && setNames([]));
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
  }, [open]);

  const choose = (name: string) => {
    setProject(name);
    setOpen(false);
  };

  return (
    <div className={`language-menu project-switcher ${open ? 'open' : ''}`} ref={menuRef}>
      <Button
        variant="ghost"
        size="sm"
        onClick={() => setOpen((prev) => !prev)}
        title={t('substrate.switcher_title')}
        aria-label={t('substrate.switcher_title')}
        aria-haspopup="menu"
        aria-expanded={open}
      >
        <IconSidebarProjects size={16} />
        <span className="project-switcher-label">{project || t('substrate.switcher_none')}</span>
      </Button>
      {open && (
        <div className="notification entering language-menu-popover" role="menu">
          {names === null && <div className="language-menu-option">{t('common.loading')}</div>}
          {names?.length === 0 && (
            <div className="language-menu-option">{t('substrate.switcher_empty')}</div>
          )}
          {names?.map((name) => (
            <button
              key={name}
              type="button"
              className={`language-menu-option ${project === name ? 'active' : ''}`}
              role="menuitemradio"
              aria-checked={project === name}
              onClick={() => choose(name)}
            >
              <span>{name}</span>
              {project === name ? <span className="language-menu-check">✓</span> : null}
            </button>
          ))}
          <button
            type="button"
            className="language-menu-option"
            role="menuitem"
            onClick={() => {
              setOpen(false);
              navigate('/projects');
            }}
          >
            <span>{t('substrate.switcher_manage')}</span>
          </button>
        </div>
      )}
    </div>
  );
}
