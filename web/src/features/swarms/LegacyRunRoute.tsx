import { Navigate, useLocation, useParams } from 'react-router-dom';
import { legacyRunPath } from './routes';

/** Раздела Workflows нет (#217): ссылки на прогоны разошлись по задачам и
 * комментариям, поэтому старый адрес прогона ведёт на страницу его роя,
 * адрес раздела — к списку роёв. */
export function LegacyRunRoute() {
  const { id } = useParams();
  const { search } = useLocation();
  return <Navigate to={legacyRunPath(id, search)} replace />;
}
