import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useLocation, useNavigate, type Location } from 'react-router-dom';
import { useProjectStore } from '@/stores';
import {
  CLOSED_PANEL,
  PANEL_ENTRY,
  closeTarget,
  followPanel,
  readSelection,
  withSelection,
  type PanelState,
  type SelectionChange,
  type SelectionParam,
} from './address';

/** Выбор страницы -- из её адреса (#194). open -- строка списка открывает
 * панель новой записью истории, change -- смена фильтра новой записью,
 * replace -- выбор на месте текущей записи (радиогруппа каталога, #270:
 * стрелки не копят записей и не прокручивают страницу к началу), close --
 * закрытие панели (closeTarget). Location -- слоя страницы: во время
 * перехода уходящая страница видит свой адрес, а не новый. */
export function useObjectSelection() {
  const location = useLocation();
  const navigate = useNavigate();
  const { search, state } = location;
  const selection = useMemo(() => readSelection(search), [search]);
  // Второе закрытие той же записи (сохранение и onClose листа) не уводит
  // шагом назад ещё раз, за пределы страницы.
  const closed = useRef<Location | null>(null);

  const open = useCallback(
    (changes: SelectionChange) =>
      navigate({ search: withSelection(search, changes) }, { state: PANEL_ENTRY }),
    [navigate, search]
  );
  const change = useCallback(
    (changes: SelectionChange) => navigate({ search: withSelection(search, changes) }),
    [navigate, search]
  );
  const replace = useCallback(
    (changes: SelectionChange) =>
      navigate({ search: withSelection(search, changes) }, { replace: true }),
    [navigate, search]
  );
  const close = useCallback(
    (param: SelectionParam) => {
      if (!readSelection(search)[param] || closed.current === location) return;
      closed.current = location;
      const target = closeTarget(state, search, param);
      if ('back' in target) navigate(-1);
      else navigate({ search: target.search });
    },
    [location, navigate, search, state]
  );
  return { selection, open, change, replace, close };
}

/** Панель объекта следует за адресом (followPanel); состояние меняется во
 * время рендера, чтобы первый же кадр показал панель нужного объекта. */
export function usePanel(selected: string | undefined): PanelState {
  const [panel, setPanel] = useState<PanelState>(CLOSED_PANEL);
  const next = followPanel(panel, selected);
  if (next !== panel) setPanel(next);
  return next;
}

/** Проект из адреса (#194) становится глобальным контекстом, как у старых
 * ссылок с проектом. Он принимается при появлении или смене параметра, а не
 * при каждом расхождении: смена проекта в шапке, пока адрес ещё несёт
 * прежний, его не возвращает. true -- проект адреса ещё не выбран. */
export function useAddressedProject(enabled: boolean): boolean {
  const { search } = useLocation();
  const requested = enabled ? (readSelection(search).project ?? '') : '';
  const selected = useProjectStore((state) => state.project);
  const setProject = useProjectStore((state) => state.setProject);
  useEffect(() => {
    if (requested) setProject(requested);
  }, [requested, setProject]);
  return requested !== '' && requested !== selected;
}
