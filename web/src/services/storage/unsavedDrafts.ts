// Несохранённые черновики редакторов вне жизненного цикла их страниц.
// memory живёт до перезагрузки вкладки и годится для секретов (config.yaml);
// session переживает перезагрузку через sessionStorage и только для данных
// без секретов. Выход из сессии очищает оба.

export type DraftPersistence = 'memory' | 'session';

export type DraftStore = {
  retain: (key: string, value: unknown, persistence: DraftPersistence) => void;
  read: (key: string) => unknown;
  discard: (key: string) => void;
  clear: () => void;
  /** Меняется при clear(): редактор прошлой сессии не пишет в следующую. */
  generation: () => number;
};

const PREFIX = 'brood.unsaved-draft:';

export function createDraftStore(getStorage: () => Storage): DraftStore {
  const memory = new Map<string, unknown>();
  let generation = 0;
  // Хранилище может быть запрещено; черновик тогда живёт только в памяти вкладки.
  const withStorage = <T>(action: (storage: Storage) => T): T | undefined => {
    try {
      return action(getStorage());
    } catch {
      return undefined;
    }
  };

  return {
    retain(key, value, persistence) {
      memory.set(key, value);
      withStorage((storage) => {
        if (persistence === 'session') storage.setItem(PREFIX + key, JSON.stringify(value));
        else storage.removeItem(PREFIX + key);
      });
    },
    read(key) {
      if (memory.has(key)) return memory.get(key);
      return withStorage((storage) => {
        const stored = storage.getItem(PREFIX + key);
        return stored === null ? undefined : (JSON.parse(stored) as unknown);
      });
    },
    discard(key) {
      memory.delete(key);
      withStorage((storage) => storage.removeItem(PREFIX + key));
    },
    clear() {
      generation += 1;
      memory.clear();
      withStorage((storage) => {
        const keys = Array.from({ length: storage.length }, (_, index) => storage.key(index));
        keys.forEach((key) => key?.startsWith(PREFIX) && storage.removeItem(key));
      });
    },
    generation: () => generation,
  };
}

export const unsavedDrafts = createDraftStore(() => window.sessionStorage);
