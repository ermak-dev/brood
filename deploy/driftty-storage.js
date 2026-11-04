(() => {
  function memoryStorage() {
    const values = new Map();
    return {
      get length() { return values.size; },
      getItem(key) { return values.get(String(key)) ?? null; },
      setItem(key, value) { values.set(String(key), String(value)); },
      removeItem(key) { values.delete(String(key)); },
      clear() { values.clear(); },
      key(index) { return [...values.keys()][index] ?? null; },
    };
  }
  for (const name of ['localStorage', 'sessionStorage']) {
    try {
      void window[name];
    } catch {
      // У actor-view opaque origin: настройки живут только до закрытия вкладки.
      Object.defineProperty(window, name, { configurable: true, value: memoryStorage() });
    }
  }
})();
