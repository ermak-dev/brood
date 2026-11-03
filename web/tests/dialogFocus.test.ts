// BUG: Переход в редактор возвращал фокус в таблицу, Escape закрывал оба слоя (#157).
// SPEC: Клавиатуру получает верхний диалог; закрытие нижнего не крадёт его фокус.
import { afterEach, expect, test } from 'bun:test';
import { isTopmostDialog, restoreDialogFocus } from '../src/components/ui/dialogFocus';

const originalDocument = Object.getOwnPropertyDescriptor(globalThis, 'document');
afterEach(() => {
  if (originalDocument) Object.defineProperty(globalThis, 'document', originalDocument);
  else Reflect.deleteProperty(globalThis, 'document');
});

function target(connected = true) {
  let calls = 0;
  return {
    node: { isConnected: connected, focus: () => calls++ } as unknown as HTMLElement,
    calls: () => calls,
  };
}

function dialogs(...contents: HTMLElement[][]) {
  const nodes = contents.map((children) => ({ contains: (node: HTMLElement) => children.includes(node) } as HTMLElement));
  Object.defineProperty(globalThis, 'document', {
    configurable: true,
    value: { querySelectorAll: () => ({ length: nodes.length, item: (index: number) => nodes[index] ?? null }) },
  });
  return nodes;
}

test('only the last modal receives keyboard events', () => {
  const [panel, confirmation] = dialogs([], []);
  expect(isTopmostDialog(panel)).toBe(false);
  expect(isTopmostDialog(confirmation)).toBe(true);
  expect(isTopmostDialog(null)).toBe(false);
});

test('an outgoing panel cannot restore background focus over its replacement editor', () => {
  const row = target();
  dialogs([]);
  restoreDialogFocus(row.node);
  expect(row.calls()).toBe(0);
});

test('closing a confirmation restores focus inside the underlying panel', () => {
  const action = target();
  dialogs([action.node]);
  restoreDialogFocus(action.node);
  expect(action.calls()).toBe(1);
});

test('closing the last dialog restores the row unless it was removed', () => {
  const row = target(), removed = target(false);
  dialogs();
  restoreDialogFocus(row.node);
  restoreDialogFocus(removed.node);
  restoreDialogFocus(null);
  expect(row.calls()).toBe(1);
  expect(removed.calls()).toBe(0);
});
