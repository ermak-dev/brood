/**
 * Markdown (#252): отчёты шагов, запросы и задачи проходов — текст моделей и
 * операторов, который панель показывает, но не исполняет. GFM через
 * react-markdown + remark-gfm; raw HTML становится текстом ещё до
 * rehype-sanitize, чтобы ни один тег автора (и закрывающий тег конверта
 * `</cross-session-message>`) не стал элементом и не пропал. Заголовки
 * понижены: отчёт внутри карточки не спорит с заголовками страницы.
 * Картинки не загружаются — только их подпись: отчёт не должен ходить во
 * внешнюю сеть из панели. `collapsible` сворачивает длинный текст до
 * ~600 px с кнопкой «Показать полностью».
 */
import {
  createElement,
  useEffect,
  useId,
  useRef,
  useState,
  type AnchorHTMLAttributes,
  type HTMLAttributes,
  type ReactNode,
} from 'react';
import { useTranslation } from 'react-i18next';
import ReactMarkdown, { type Components } from 'react-markdown';
import remarkGfm from 'remark-gfm';
import rehypeSanitize, { defaultSchema } from 'rehype-sanitize';
import { Button } from './Button';
import styles from './Markdown.module.scss';

/** Высота свёрнутого текста; измеряется после монтирования. */
const COLLAPSED_HEIGHT = 600;
/** Оценка «длинный» до измерения (серверный рендер, первый кадр):
 * ~30 строк по 13px/1.5 или столько же текста при 72ch. */
const LONG_LINES = 28;
const LONG_CHARS = 2200;

interface HastNode {
  type: string;
  value?: string;
  children?: HastNode[];
}

/** Rehype-плагин: узлы raw (HTML автора) — в текст, до sanitize, который их
 * иначе просто выбросил бы. Без зависимости от unist-util-visit. */
function rawToText() {
  const walk = (node: HastNode) => {
    if (!node.children) return;
    for (const child of node.children) {
      if (child.type === 'raw') {
        child.type = 'text';
        delete child.children;
      } else {
        walk(child);
      }
    }
  };
  return (tree: HastNode) => walk(tree);
}

const rehypePlugins: NonNullable<Parameters<typeof ReactMarkdown>[0]['rehypePlugins']> = [
  rawToText,
  [rehypeSanitize, defaultSchema],
];
const remarkPlugins = [remarkGfm];

type Props<T extends keyof React.JSX.IntrinsicElements> = React.JSX.IntrinsicElements[T] & {
  node?: unknown;
};

function lowered(tag: 'h4' | 'h5' | 'h6') {
  return function Heading({ node: _node, ...props }: Props<'h1'>) {
    void _node;
    return createElement(tag, props);
  };
}

function Anchor({ node: _node, ...props }: Props<'a'>) {
  void _node;
  const anchor: AnchorHTMLAttributes<HTMLAnchorElement> = {
    ...props,
    target: '_blank',
    rel: 'noopener noreferrer',
  };
  return <a {...anchor} />;
}

function Table({ node: _node, ...props }: Props<'table'>) {
  void _node;
  return (
    <div className={styles.tableWrap}>
      <table {...props} />
    </div>
  );
}

/** Картинка — подписью: панель не загружает внешние ресурсы по воле отчёта. */
function Image({ alt, src }: Props<'img'>) {
  return <span className={styles.image}>{alt || String(src ?? '')}</span>;
}

const components: Components = {
  h1: lowered('h4'),
  h2: lowered('h5'),
  h3: lowered('h6'),
  h4: lowered('h6'),
  h5: lowered('h6'),
  h6: lowered('h6'),
  a: Anchor,
  table: Table,
  img: Image,
};

function looksLong(text: string): boolean {
  return text.length > LONG_CHARS || text.split('\n').length > LONG_LINES;
}

export interface MarkdownProps extends Omit<HTMLAttributes<HTMLDivElement>, 'children'> {
  /** Текст в GFM, как прислан. */
  text: string;
  /** Сворачивать длинный текст до COLLAPSED_HEIGHT с кнопкой раскрытия. */
  collapsible?: boolean;
}

export function Markdown({ text, collapsible = false, className, ...rest }: MarkdownProps) {
  const { t } = useTranslation();
  const bodyId = useId();
  const body = useRef<HTMLDivElement>(null);
  const [tall, setTall] = useState(() => collapsible && looksLong(text));
  const [expanded, setExpanded] = useState(false);

  // Оценка до измерения — текстом; после монтирования — реальной высотой.
  useEffect(() => {
    if (!collapsible || !body.current) return;
    setTall(body.current.scrollHeight > COLLAPSED_HEIGHT + 40);
  }, [collapsible, text]);

  const collapsed = tall && !expanded;
  return (
    <div className={[styles.root, className].filter(Boolean).join(' ')} {...rest}>
      <div
        id={bodyId}
        ref={body}
        className={styles.body}
        data-collapsed={collapsed ? '' : undefined}
      >
        <ReactMarkdown
          remarkPlugins={remarkPlugins}
          rehypePlugins={rehypePlugins}
          components={components}
        >
          {text}
        </ReactMarkdown>
      </div>
      {tall && (
        <Button
          type="button"
          variant="ghost"
          size="sm"
          className={styles.toggle}
          aria-expanded={expanded}
          aria-controls={bodyId}
          onClick={() => setExpanded((value) => !value)}
        >
          {expanded ? t('common.collapse') : t('common.show_full')}
        </Button>
      )}
    </div>
  );
}

export type DetailSegment = string | { id: string };

/** Причина шага (`detail`) остаётся текстом, как прислал сервер; размечается
 * только ID операции после слова `operation` — его оператор копирует в
 * `bro dispatch ... --request-id`. */
export function operationIdSegments(detail: string): DetailSegment[] {
  const segments: DetailSegment[] = [];
  const pattern = /\bworkspace operation ([A-Za-z0-9][\w-]*)/g;
  let last = 0;
  for (const match of detail.matchAll(pattern)) {
    const start = (match.index ?? 0) + 'workspace operation '.length;
    segments.push(detail.slice(last, start), { id: match[1] });
    last = start + match[1].length;
  }
  const tail = detail.slice(last);
  if (tail || segments.length === 0) segments.push(tail);
  return segments;
}

export function PlainText({ text, className }: { text: string; className?: string }) {
  const children: ReactNode[] = operationIdSegments(text).map((segment, index) =>
    typeof segment === 'string' ? (
      segment
    ) : (
      <code key={index} className={styles.id}>
        {segment.id}
      </code>
    )
  );
  return <p className={className}>{children}</p>;
}
