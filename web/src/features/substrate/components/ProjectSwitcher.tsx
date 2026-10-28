import { useEffect, useRef, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { IconSidebarProjects } from '@/components/ui/icons';
import { useProjectStore } from '@/stores';
import { scopesApi, type SwarmScope } from '@/services/brood/substrate';

/** brood: the active swarm switcher in the header, beside the language menu
 * and in its popover style -- the one global scope of the panel. Actors,
 * templates and the project-scoped policy are scoped to it; a swarm is listed
 * as "project / swarm" because its name is unique only inside its project. The
 * list is fetched each time the menu opens, so a swarm created a moment ago is
 * there. */
export function ProjectSwitcher() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const project = useProjectStore((state) => state.project);
  const setProject = useProjectStore((state) => state.setProject);
  const [open, setOpen] = useState(false);
  const [scopes, setScopes] = useState<SwarmScope[] | null>(null);
  const menuRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    let current = true;
    scopesApi
      .list()
      .then((items) => current && setScopes(items))
      .catch(() => current && setScopes([]));
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
        title={t('access.select_swarm')}
        aria-label={t('access.select_swarm')}
        aria-haspopup="menu"
        aria-expanded={open}
      >
        <IconSidebarProjects size={16} />
        <span className="project-switcher-label">
          {scopes?.find((scope) => scope.metadata.name === project)?.label ||
            t('access.select_swarm')}
        </span>
      </Button>
      {open && (
        <div className="notification entering language-menu-popover" role="menu">
          {scopes === null && <div className="language-menu-option">{t('common.loading')}</div>}
          {scopes?.length === 0 && <div className="language-menu-option">{t('access.empty')}</div>}
          {scopes?.map(({ metadata: { name }, label }) => (
            <button
              key={name}
              type="button"
              className={`language-menu-option ${project === name ? 'active' : ''}`}
              role="menuitemradio"
              aria-checked={project === name}
              onClick={() => choose(name)}
            >
              <span>{label}</span>
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
            <span>{t('access.projects')}</span>
          </button>
        </div>
      )}
    </div>
  );
}
