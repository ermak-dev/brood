import type { ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { objectPath, type ObjectRef } from './address';

/** Подпись по умолчанию -- имя объекта; у роя и прогона имени в ссылке нет,
 * его называет вызывающий, иначе остаётся ID. */
function labelOf(object: ObjectRef): string {
  switch (object.kind) {
    case 'swarm':
      return object.id;
    case 'run':
    case 'actor':
    case 'template':
      return 'name' in object && object.name ? object.name : object.swarm;
    default:
      return object.name;
  }
}

/** Ссылка на объект brood любого типа (#194): рой, прогон, актор, шаблон,
 * воркер, пул. Адрес -- из objectPath, страница по нему выбирает проект и рой
 * и открывает панель объекта. Связи (#195) и обзор проекта (#197) ссылаются
 * на объекты только так. */
export function ObjectLink({
  object,
  children,
  className,
}: {
  object: ObjectRef;
  children?: ReactNode;
  className?: string;
}) {
  return (
    <Link to={objectPath(object)} className={className}>
      {children ?? labelOf(object)}
    </Link>
  );
}
