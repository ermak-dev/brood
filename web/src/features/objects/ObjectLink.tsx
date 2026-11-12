import type { ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { DASH } from '@/features/substrate/format';
import { objectPath, type ObjectRef } from './address';
import type { Related } from './relations';
import styles from './ObjectLink.module.scss';

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
 * воркер, пул, определение workflow (#270). Адрес -- из objectPath, страница
 * по нему выбирает проект и рой и открывает панель объекта, определение --
 * в каталоге страницы проекта. Связи (#195) и обзор проекта (#197) ссылаются
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
    <Link
      to={objectPath(object)}
      className={className ? `${styles.objectLink} ${className}` : styles.objectLink}
    >
      {children ?? labelOf(object)}
    </Link>
  );
}

/** Связанный объект (#195): ссылка, когда его страница откроется, иначе имя
 * текстом; связи нет -- прочерк. */
export function RelatedLink({ related }: { related: Related | undefined }) {
  if (!related) return <>{DASH}</>;
  if (!related.ref) return <>{related.name}</>;
  return <ObjectLink object={related.ref}>{related.name}</ObjectLink>;
}

/** Несколько связанных объектов строкой с переносом: акторы шаблона,
 * воркеры пула. */
export function RelatedList({ items }: { items: Related[] }) {
  return (
    <ul className={styles.relatedList}>
      {items.map((item, index) => (
        <li key={`${index}-${item.name}`}>
          <RelatedLink related={item} />
        </li>
      ))}
    </ul>
  );
}
