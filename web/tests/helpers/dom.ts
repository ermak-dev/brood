/**
 * Общий тестовый DOM (#214): окно happy-dom задаёт DOMParser и document
 * глобально для bun:test — ровно на время файла, который это подключил.
 * Реальному приложению глобалы даёт браузер; а другим тестам нужен
 * «документ не определён», чтобы компоненты вроде Modal уходили в ветку
 * серверного рендера вместо портала. Поэтому импорт возвращает функцию
 * включения, а хук afterAll возвращает как было.
 */
import { Window } from 'happy-dom';

export interface TestDOM {
  /** Реализация DOM для парсера. */
  impl: DocumentImplementationLike;
}

export interface DocumentImplementationLike {
  createHTMLDocument: (title?: string) => Document;
}

const window = new Window();

/** Включает DOM-глобалы и возвращает функцию восстановления. */
export function installTestDOM(): () => void {
  const hadParser = 'DOMParser' in globalThis;
  const hadDocument = 'document' in globalThis;
  const priorParser = (globalThis as { DOMParser?: unknown }).DOMParser;
  const priorDocument = (globalThis as { document?: unknown }).document;
  globalThis.DOMParser = window.DOMParser as unknown as typeof DOMParser;
  if (!hadDocument) {
    globalThis.document = window.document as unknown as Document;
  }
  // React DOM (клиент) читает window и конструкторы событий из глобалов.
  const extra = [
    'window',
    'HTMLElement',
    'Element',
    'Node',
    'KeyboardEvent',
    'MouseEvent',
    'Event',
  ];
  const g = globalThis as Record<string, unknown>;
  const saved = extra.map((k) => [k, k in g, g[k]] as const);
  for (const k of extra) {
    if (!(k in g))
      g[k] = k === 'window' ? window : (window as unknown as Record<string, unknown>)[k];
  }
  return () => {
    for (const [k, had, value] of saved) {
      if (had) g[k] = value;
      else delete g[k];
    }
    if (hadParser) {
      (globalThis as { DOMParser?: unknown }).DOMParser = priorParser;
    } else {
      delete (globalThis as { DOMParser?: unknown }).DOMParser;
    }
    if (!hadDocument) {
      delete (globalThis as { document?: unknown }).document;
    } else {
      (globalThis as { document?: unknown }).document = priorDocument;
    }
  };
}
