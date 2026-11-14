// FEATURE: Telegram section of the user profile in the panel (#285, API #281, epic #278).
// SPEC: /profile is open to every signed-in user and reached from the header.
// Its Telegram section shows the bot the link goes through (own or the
// installation bot) and the state, the redacted reason of an error, the
// linked chat, and the subscriptions. The token field is write-only: a typed
// token is only the value of the field, never in the markup (the value
// attribute included) or browser storage; after a save the field is empty and
// the page shows only @bot and the state; a refusal keeps what was typed and
// says why. A link code comes with its t.me deep link, copy actions and
// expiry; the server decides whether it waits, and a browser clock off by
// minutes neither hides it nor miscounts it. The page polls while the code
// waits and turns linked without a reload. Unlinking and removing the own
// bot are confirmed and the link can be issued again right after. A
// subscription switch writes the full set (#296, API #295) and shows what the
// server saved; a refusal is shown at the subscriptions with the server's
// reason, and the switch stays as the server has it. A switch that lost the
// focus while its write disabled it gets it back after the reply, unless the
// user moved it elsewhere. A typed,
// unsaved token blocks leaving the page like other forms. A poll that left
// before a write does not overwrite the state the write returned, a poll
// that left after it lands, and a poll or header refresh during a write
// stays silent. A write that failed after
// the server committed it shows the committed state without a reload.
import { afterAll, afterEach, beforeEach, describe, expect, spyOn, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { createInstance } from 'i18next';
import { I18nextProvider } from 'react-i18next';
import { MemoryRouter, RouterProvider, createMemoryRouter } from 'react-router-dom';
import { TELEGRAM_EVENTS } from '@/services/brood/telegram';
import { visibleAccessPath } from '@/features/access/permissions';
import { ProfilePage } from '@/features/profile/ProfilePage';
import { ProfileLink } from '@/features/profile/ProfileLink';
import { PROFILE_PATH } from '@/features/profile/routes';
import { TELEGRAM_POLL_MS, tickTelegramPolling } from '@/features/profile/useTelegramProfile';
import { triggerHeaderRefresh } from '@/hooks/useHeaderRefresh';
import { useAuthStore, useNotificationStore } from '@/stores';
import { installTestDOM } from './helpers/dom';
import en from '@/i18n/locales/en.json';

const restoreDOM = installTestDOM();
// Оба хранилища браузера -- карты в памяти: проверки ниже смотрят, что
// токена в них нет, а не падают на отсутствующем sessionStorage.
const STORAGES = ['localStorage', 'sessionStorage'] as const;
const priorStorage = STORAGES.map(
  (name) => [name, Object.getOwnPropertyDescriptor(globalThis, name)] as const
);
const mapStorage = (memory: Map<string, string>): Storage => ({
  get length() {
    return memory.size;
  },
  key: (index: number) => [...memory.keys()][index] ?? null,
  getItem: (key: string) => memory.get(key) ?? null,
  setItem: (key: string, value: string) => void memory.set(key, String(value)),
  removeItem: (key: string) => void memory.delete(key),
  clear: () => memory.clear(),
});
for (const name of STORAGES) {
  Object.defineProperty(globalThis, name, {
    configurable: true,
    value: mapStorage(new Map()),
  });
}
afterAll(() => {
  restoreDOM();
  for (const [name, prior] of priorStorage) {
    if (prior) Object.defineProperty(globalThis, name, prior);
    else Reflect.deleteProperty(globalThis, name);
  }
});
(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const i18n = createInstance();
await i18n.init({
  lng: 'en',
  resources: { en: { translation: en } },
  interpolation: { escapeValue: false },
});
const t = en.profile.telegram;
const fill = (template: string, values: Record<string, string | number>) =>
  template.replace(/\{\{(\w+)\}\}/g, (_, key: string) => String(values[key]));

const TOKEN = '123456789:AAH-synthetic_bot_token_for_tests_only';
const SECRET = TOKEN.split(':')[1];
const CODE = 'Zk3vQx2b8m1nP0aR7sT4uV6wX9yZ5cD3eF8gH1iJ2kL';
const EVENTS_PATH = '/api/me/telegram/events';

/** Где токен остался бы вне значения поля: хранилища браузера (глобальные
 * и окна DOM) и разметка страницы, вместе с атрибутом value поля -- её
 * читает всё, что сериализует DOM. */
function tokenTraces(): string[] {
  const g = globalThis as unknown as Record<string, Storage | undefined>;
  const w = window as unknown as Record<string, Storage | undefined>;
  const found: string[] = [];
  for (const name of STORAGES) {
    for (const [where, store] of [
      ['global', g[name]],
      ['window', w[name]],
    ] as const) {
      for (let i = 0; store && i < store.length; i++) {
        const key = store.key(i) ?? '';
        if (`${key}=${store.getItem(key)}`.includes(SECRET)) found.push(`${where} ${name} ${key}`);
      }
    }
  }
  if (document.documentElement.outerHTML.includes(SECRET)) found.push('markup');
  return found;
}
// Часы поддельного сервера -- настоящие: тест сдвигает часы браузера
// (Date.now), а сервер их не видит.
const serverNow = Date.now.bind(Date);
const inMinutes = (minutes: number) =>
  new Date(Math.floor(serverNow() / 1000) * 1000 + minutes * 60_000)
    .toISOString()
    .replace('.000', '');
/** Ответ сервера с его часами в Date, как у net/http. */
const reply = (body: unknown, status = 200) =>
  Response.json(body, { status, headers: { Date: new Date(serverNow()).toUTCString() } });

const installUnlinked = () => ({
  bot_username: 'brood_bot',
  own_bot: false,
  state: 'unlinked',
  events: [...TELEGRAM_EVENTS],
  chat: null,
  link_code_pending: false,
});

interface Call {
  method: string;
  url: string;
  body?: string;
}

/** Поддельный API профиля: состояние сервера и записанные вызовы. GET можно
 * задержать, чтобы проверить порядок ответов; запись -- тоже, и тогда
 * сервер фиксирует её только при отпуске. failAfterCommit -- запись
 * зафиксирована, а ответ -- сбой (сверка поллеров не прошла). newerKinds --
 * виды словаря сервера новее панели. */
const server = {
  profile: installUnlinked() as Record<string, unknown>,
  newerKinds: [] as string[],
  calls: [] as Call[],
  refusal: null as null | { status: number; reason: string },
  held: null as null | ((profile: Record<string, unknown>) => void),
  holdNextGet: false,
  releaseWrite: null as null | (() => void),
  holdNextWrite: false,
  failAfterCommit: false,
};

/** Запись сервера: правит его состояние и отвечает. Подписки сервер
 * хранит в порядке своего словаря, как store.SetTelegramEvents; вид вне
 * словаря -- отказ #295, и запись ничего не пишет. */
function commit(method: string, url: string, body?: string): Response {
  if (method === 'PUT' && url === EVENTS_PATH) {
    const known = [...TELEGRAM_EVENTS, ...server.newerKinds];
    const sent = (JSON.parse(body ?? '{}') as { events: string[] }).events;
    if (sent.some((kind) => !known.includes(kind))) {
      const reason = `unknown telegram event; known events: ${known.join(', ')}`;
      return reply({ detail: reason, reason }, 400);
    }
    server.profile = { ...server.profile, events: known.filter((kind) => sent.includes(kind)) };
  } else if (method === 'PUT') {
    server.profile = { ...server.profile, bot_username: 'alice_dev_bot', own_bot: true };
  }
  if (method === 'DELETE') {
    server.profile = installUnlinked();
  }
  if (method === 'POST') {
    const expires = inMinutes(15);
    server.profile = {
      ...server.profile,
      state: 'unlinked',
      link_code_pending: true,
      link_code_expires_at: expires,
    };
    delete server.profile.reason;
    const bot = server.profile.bot_username as string;
    if (server.failAfterCommit) return reply({ detail: 'internal error' }, 500);
    return reply(
      {
        code: CODE,
        expires_at: expires,
        bot_username: bot,
        ...(bot ? { url: `https://t.me/${bot}?start=${CODE}` } : {}),
      },
      201
    );
  }
  if (server.failAfterCommit) return reply({ detail: 'internal error' }, 500);
  return reply(server.profile);
}

const originalFetch = globalThis.fetch;
beforeEach(() => {
  server.profile = installUnlinked();
  server.newerKinds = [];
  server.calls = [];
  server.refusal = null;
  server.held = null;
  server.holdNextGet = false;
  server.releaseWrite = null;
  server.holdNextWrite = false;
  server.failAfterCommit = false;
  useAuthStore.setState({ username: 'alice', isSuperuser: false });
  useNotificationStore.setState({
    confirmation: { isOpen: false, isLoading: false, options: null },
  });
  document.cookie = 'csrf=telegram-ui';
  globalThis.fetch = (async (input: string | URL | Request, init?: RequestInit) => {
    const url = String(input);
    const method = init?.method ?? 'GET';
    const body = init?.body as string | undefined;
    server.calls.push({ method, url, body });
    if (url !== '/api/me/telegram' && url !== '/api/me/telegram/link' && url !== EVENTS_PATH) {
      return reply({ detail: 'not found' }, 404);
    }
    if (server.refusal && method !== 'GET') {
      const { status, reason } = server.refusal;
      return reply({ detail: reason, reason }, status);
    }
    if (method === 'GET' && server.holdNextGet) {
      server.holdNextGet = false;
      return new Promise<Response>((resolve) => {
        server.held = (profile) => resolve(reply(profile));
      });
    }
    if (method === 'GET') return reply(server.profile);
    if (server.holdNextWrite) {
      server.holdNextWrite = false;
      return new Promise<Response>((resolve) => {
        server.releaseWrite = () => resolve(commit(method, url, body));
      });
    }
    return commit(method, url, body);
  }) as typeof fetch;
});
afterEach(() => {
  globalThis.fetch = originalFetch;
});

interface Mounted {
  container: HTMLElement;
  root: Root;
  router: ReturnType<typeof createMemoryRouter>;
}

async function mount(): Promise<Mounted> {
  const container = document.createElement('div');
  document.body.appendChild(container);
  const root = createRoot(container);
  const router = createMemoryRouter(
    [
      { path: PROFILE_PATH, element: createElement(ProfilePage) },
      { path: '/swarms', element: createElement('p', null, 'swarm list') },
    ],
    { initialEntries: [PROFILE_PATH] }
  );
  await act(async () =>
    root.render(createElement(I18nextProvider, { i18n }, createElement(RouterProvider, { router })))
  );
  await settle();
  return { container, root, router };
}

async function unmount({ container, root }: Mounted) {
  await act(async () => root.unmount());
  container.remove();
}

const settle = (ms = 0) => act(async () => new Promise((resolve) => setTimeout(resolve, ms)));
const text = (scope: ParentNode) => (scope as HTMLElement).textContent?.replace(/\s+/g, ' ') ?? '';
const buttonNamed = (scope: ParentNode | null | undefined, name: string) =>
  [...(scope?.querySelectorAll('button') ?? [])].find(
    (button) => button.textContent?.trim() === name || button.getAttribute('aria-label') === name
  ) as HTMLButtonElement | undefined;
const click = (element: Element | null | undefined) =>
  act(async () => {
    if (!element) throw new Error('nothing to click');
    (element as HTMLElement).click();
  });
const dialog = () => document.querySelector('[role="dialog"]') as HTMLElement | null;
const tokenField = (container: HTMLElement) =>
  container.querySelector('input[name="telegram-bot-token"]') as HTMLInputElement;
const tokenForm = (container: HTMLElement) =>
  tokenField(container).closest('form') as HTMLFormElement;

/** Ввод так, как его видит React: bun грузит react-dom раньше DOM теста, и
 * событие input до onChange не доходит (см. swarmRequestsUi.test.ts). */
async function type(field: HTMLInputElement, value: string) {
  await act(async () => {
    const setter = Object.getOwnPropertyDescriptor(
      Object.getPrototypeOf(field) as object,
      'value'
    )?.set;
    setter?.call(field, value);
    const props = Object.entries(field).find(([key]) => key.startsWith('__reactProps$'))?.[1] as
      { onChange?: (event: { target: HTMLInputElement }) => void } | undefined;
    props?.onChange?.({ target: field });
  });
}

async function submit(form: HTMLFormElement) {
  await act(async () => {
    form.dispatchEvent(new window.Event('submit', { bubbles: true, cancelable: true }));
  });
  await settle();
}

async function confirmDialog() {
  const form = dialog()?.querySelector('form');
  if (!form) throw new Error('no dialog form');
  await submit(form as HTMLFormElement);
}

const writes = () =>
  server.calls.filter((call) => call.method !== 'GET').map((call) => `${call.method} ${call.url}`);
/** Наборы подписок, отправленные записью, по порядку. */
const sentEvents = () =>
  server.calls
    .filter((call) => call.method === 'PUT' && call.url === EVENTS_PATH)
    .map((call) => (JSON.parse(call.body ?? '') as { events: string[] }).events);

const eventToggles = (container: HTMLElement) =>
  [...container.querySelectorAll('input[type="checkbox"]')] as HTMLInputElement[];
const eventToggle = (container: HTMLElement, label: string) => {
  const toggle = eventToggles(container).find((item) => text(item.closest('label')!) === label);
  if (!toggle) throw new Error(`no switch ${label}`);
  return toggle;
};
/** Подписи включённых переключателей подписок. */
const subscribed = (container: HTMLElement) =>
  eventToggles(container)
    .filter((toggle) => toggle.checked)
    .map((toggle) => text(toggle.closest('label')!));

/** Щелчок переключателя и ответ записи. */
async function flip(toggle: HTMLInputElement) {
  await click(toggle);
  await settle();
}

/** Что в фокусе: подпись переключателя, BODY или тег с текстом. Строка, а
 * не узел: отказ сравнения узлов печатает всё дерево React. */
const focused = () => {
  const active = document.activeElement as HTMLElement | null;
  const label = active?.closest('label');
  if (label) return text(label);
  if (!active || active === document.body) return 'BODY';
  return `${active.tagName} ${text(active)}`;
};
/** Фокус уходит на body, как у браузера с выключенного элемента в фокусе:
 * happy-dom сам его не уводит, а blur выключенного не принимает. */
const loseFocus = () => document.body.focus();

describe('route and entry', () => {
  test('/profile is a route of its own, open to members, and the header leads to it', () => {
    const routes = readFileSync(new URL('../src/router/MainRoutes.tsx', import.meta.url), 'utf8');
    expect(PROFILE_PATH).toBe('/profile');
    expect(routes).toContain('{ path: PROFILE_PATH, element: <ProfilePage /> }');
    expect(visibleAccessPath(PROFILE_PATH, false)).toBe(true);
    expect(visibleAccessPath(PROFILE_PATH, true)).toBe(true);
    const layout = readFileSync(
      new URL('../src/components/layout/MainLayout.tsx', import.meta.url),
      'utf8'
    );
    expect(layout).toContain('<ProfileLink />');
  });

  test('the header link names the profile and the signed-in user', async () => {
    // Живой рендер: серверный берёт начальное состояние хранилища, без входа.
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    await act(async () =>
      root.render(
        createElement(
          I18nextProvider,
          { i18n },
          createElement(MemoryRouter, null, createElement(ProfileLink))
        )
      )
    );
    const link = container.querySelector('a');
    expect(link?.getAttribute('href')).toBe('/profile');
    expect(link?.getAttribute('aria-label')).toBe(fill(en.header.profile_of, { user: 'alice' }));
    await act(async () => root.unmount());
    container.remove();
  });
});

describe('profile page', () => {
  test('shows who is signed in and the bot the link goes through', async () => {
    const page = await mount();
    const content = text(page.container);
    expect(page.container.querySelector('h1')?.textContent).toBe(en.profile.title);
    expect(content).toContain('alice');
    expect(content).toContain(en.profile.role_member);
    expect(page.container.querySelector('h2')?.textContent).toBe(t.title);
    expect(content).toContain('@brood_bot');
    expect(content).toContain(t.installation_bot);
    expect(content).toContain(t.state.unlinked);
    expect(content).toContain(t.chat_none);
    await unmount(page);
  });

  test('the token field is a password field with no value from the server', async () => {
    const page = await mount();
    const field = tokenField(page.container);
    expect(field.type).toBe('password');
    expect(field.getAttribute('autocomplete')).toBe('new-password');
    expect(field.value).toBe('');
    expect(text(page.container)).toContain(t.token_write_only);
    await unmount(page);
  });
});

describe('bot token', () => {
  test('a typed token is only the value of the field: not in markup, not in storage', async () => {
    const page = await mount();
    await type(tokenField(page.container), TOKEN);
    expect(tokenField(page.container).value).toBe(TOKEN);
    expect(tokenField(page.container).getAttribute('value') ?? '').not.toContain(SECRET);
    expect(tokenTraces()).toEqual([]);
    expect(buttonNamed(page.container, t.token_save)?.disabled).toBe(false);
    await unmount(page);
  });

  test('a saved token leaves the field empty and the page shows only @bot', async () => {
    const page = await mount();
    await type(tokenField(page.container), `  ${TOKEN}\n`);
    await submit(tokenForm(page.container));
    const put = server.calls.find((call) => call.method === 'PUT');
    expect(JSON.parse(put?.body ?? '')).toEqual({ token: TOKEN });
    expect(tokenField(page.container).value).toBe('');
    expect(document.body.innerHTML).not.toContain(SECRET);
    expect(tokenTraces()).toEqual([]);
    expect(buttonNamed(page.container, t.token_save)?.disabled).toBe(true);
    const content = text(page.container);
    expect(content).toContain('@alice_dev_bot');
    expect(content).toContain(t.own_bot);
    expect(content).toContain(fill(t.token_saved, { bot: '@alice_dev_bot' }));
    await unmount(page);
  });

  test('a token of the wrong shape is not sent and the field says why', async () => {
    const page = await mount();
    await type(tokenField(page.container), 'not-a-token');
    await submit(tokenForm(page.container));
    expect(writes()).toEqual([]);
    expect(text(page.container)).toContain(t.token_shape);
    await unmount(page);
  });

  test('an empty field has nothing to save', async () => {
    const page = await mount();
    expect(buttonNamed(page.container, t.token_save)?.disabled).toBe(true);
    await unmount(page);
  });

  test('a refusal keeps the typed token for a retry and shows the reason', async () => {
    const page = await mount();
    server.refusal = { status: 422, reason: 'Telegram rejected the bot token' };
    await type(tokenField(page.container), TOKEN);
    await submit(tokenForm(page.container));
    expect(tokenField(page.container).value).toBe(TOKEN);
    const alert = [...page.container.querySelectorAll('[role="alert"]')].map((node) => text(node));
    expect(alert).toContain(fill(t.refused, { reason: 'Telegram rejected the bot token' }));
    expect(tokenTraces()).toEqual([]);
    // Повтор шлёт то, что набрано, без нового ввода.
    server.refusal = null;
    await submit(tokenForm(page.container));
    const puts = server.calls.filter((call) => call.method === 'PUT');
    expect(puts.map((call) => JSON.parse(call.body ?? ''))).toEqual([
      { token: TOKEN },
      { token: TOKEN },
    ]);
    expect(tokenField(page.container).value).toBe('');
    expect(tokenTraces()).toEqual([]);
    await unmount(page);
  });
});

describe('chat link', () => {
  test('a code comes with its deep link, copy actions and expiry, and linking needs no reload', async () => {
    const page = await mount();
    await click(buttonNamed(page.container, t.link_issue));
    expect(writes()).toEqual(['POST /api/me/telegram/link']);
    const link = page.container.querySelector(`a[href="https://t.me/brood_bot?start=${CODE}"]`);
    expect(link).not.toBeNull();
    expect(link?.getAttribute('rel')).toBe('noopener noreferrer');
    expect(text(page.container)).toContain(CODE);
    expect(buttonNamed(page.container, t.copy_link)).toBeDefined();
    expect(buttonNamed(page.container, t.copy_code)).toBeDefined();
    expect(text(page.container)).toContain(fill(t.minutes_left_other, { count: 15 }));
    expect(text(page.container)).toContain(t.state.waiting);
    expect(text(page.container)).toContain(t.link_waiting);

    // Пользователь отправил /start боту: опрос видит привязку.
    server.profile = {
      ...server.profile,
      state: 'linked',
      link_code_pending: false,
      chat: { telegram_user_id: 4242, chat_id: 4242, username: 'alice_tg' },
    };
    delete server.profile.link_code_expires_at;
    await act(async () => tickTelegramPolling());
    await settle();
    const content = text(page.container);
    expect(content).toContain(fill(t.linked_notice, { chat: '@alice_tg' }));
    expect(content).toContain(t.state.linked);
    expect(content).not.toContain(CODE);
    expect(buttonNamed(page.container, t.unlink)).toBeDefined();
    expect(buttonNamed(page.container, t.link_issue)).toBeUndefined();
    await unmount(page);
  });

  test('the page polls on a timer only while a code waits for /start', async () => {
    const timers = spyOn(globalThis, 'setInterval');
    const stopped = spyOn(globalThis, 'clearInterval');
    const polls = () => timers.mock.calls.filter(([, delay]) => delay === TELEGRAM_POLL_MS).length;
    try {
      const page = await mount();
      expect(polls()).toBe(0);
      await click(buttonNamed(page.container, t.link_issue));
      expect(polls()).toBe(1);
      const stopsBefore = stopped.mock.calls.length;
      server.profile = {
        ...server.profile,
        state: 'linked',
        link_code_pending: false,
        chat: { telegram_user_id: 4242, chat_id: 4242 },
      };
      await act(async () => tickTelegramPolling());
      await settle();
      expect(stopped.mock.calls.length).toBeGreaterThan(stopsBefore);
      expect(polls()).toBe(1);
      await unmount(page);
    } finally {
      timers.mockRestore();
      stopped.mockRestore();
    }
  });

  test('a notice of the last change goes once the state changes again', async () => {
    const page = await mount();
    await click(buttonNamed(page.container, t.link_issue));
    server.profile = {
      ...server.profile,
      state: 'linked',
      link_code_pending: false,
      chat: { telegram_user_id: 4242, chat_id: 4242, username: 'alice_tg' },
    };
    await act(async () => tickTelegramPolling());
    await settle();
    const linked = fill(t.linked_notice, { chat: '@alice_tg' });
    expect(text(page.container)).toContain(linked);
    // Telegram отозвал токен: поллер перевёл привязку в ошибку.
    const reason = 'Telegram rejected the bot token (401): set a new bot token';
    server.profile = { ...server.profile, state: 'error', reason, chat: null };
    await act(async () => tickTelegramPolling());
    await settle();
    expect(text(page.container)).not.toContain(linked);
    expect(text(page.container)).toContain(fill(t.error_reason, { reason }));
    await unmount(page);
  });

  test('a browser clock off by minutes still shows the code and counts down on the server clock', async () => {
    for (const skew of [20 * 60_000, -20 * 60_000]) {
      server.profile = installUnlinked();
      const clock = spyOn(Date, 'now').mockImplementation(() => serverNow() + skew);
      const timers = spyOn(globalThis, 'setInterval');
      try {
        const page = await mount();
        await click(buttonNamed(page.container, t.link_issue));
        const content = text(page.container);
        expect({ skew, shown: content.includes(CODE) }).toEqual({ skew, shown: true });
        expect({ skew, expired: content.includes(t.link_expired) }).toEqual({
          skew,
          expired: false,
        });
        expect(content).toContain(fill(t.minutes_left_other, { count: 15 }));
        expect(content).toContain(t.state.waiting);
        // Опрос идёт, пока код ждёт по слову сервера.
        expect(timers.mock.calls.filter(([, delay]) => delay === TELEGRAM_POLL_MS)).toHaveLength(1);
        await unmount(page);
      } finally {
        clock.mockRestore();
        timers.mockRestore();
      }
    }
  });

  test('a code of a bot without a known name is sent with /start, with no link', async () => {
    const page = await mount();
    // Имя бота сервер к выдаче кода не узнал: ссылки t.me в ответе нет.
    server.profile = { ...server.profile, bot_username: '' };
    await click(buttonNamed(page.container, t.link_issue));
    expect(page.container.querySelector('a[href^="https://t.me/"]')).toBeNull();
    expect(text(page.container)).toContain(fill(t.link_manual, { code: CODE }));
    await unmount(page);
  });

  test('a code issued earlier waits without being shown again', async () => {
    server.profile = {
      ...installUnlinked(),
      link_code_pending: true,
      link_code_expires_at: inMinutes(10),
    };
    const page = await mount();
    const content = text(page.container);
    expect(content).toContain(t.link_pending_elsewhere.split('{{time}}')[0]);
    expect(content).toContain(t.state.waiting);
    expect(buttonNamed(page.container, t.link_reissue)).toBeDefined();
    await unmount(page);
  });

  test('unlinking is confirmed and the chat can be linked again right after', async () => {
    server.profile = {
      ...installUnlinked(),
      state: 'linked',
      chat: { telegram_user_id: 4242, chat_id: 4242, username: 'alice_tg' },
    };
    const page = await mount();
    expect(text(page.container)).toContain('@alice_tg');
    await click(buttonNamed(page.container, t.unlink));
    expect(text(dialog()!)).toContain(fill(t.unlink_message, { chat: '@alice_tg' }));
    expect(writes()).toEqual([]);
    await confirmDialog();
    expect(writes()).toEqual(['DELETE /api/me/telegram']);
    const content = text(page.container);
    expect(content).toContain(t.unlinked);
    expect(content).toContain(t.state.unlinked);
    expect(buttonNamed(page.container, t.link_issue)).toBeDefined();
    await unmount(page);
  });

  test('removing the own bot names it and also unlinks the chat', async () => {
    server.profile = {
      ...installUnlinked(),
      bot_username: 'alice_dev_bot',
      own_bot: true,
      state: 'linked',
      chat: { telegram_user_id: 4242, chat_id: 4242 },
    };
    const page = await mount();
    expect(text(page.container)).toContain(fill(t.chat_user, { id: 4242 }));
    await click(buttonNamed(page.container, t.remove_bot));
    expect(text(dialog()!)).toContain(fill(t.remove_bot_message, { bot: '@alice_dev_bot' }));
    await confirmDialog();
    expect(writes()).toEqual(['DELETE /api/me/telegram']);
    const content = text(page.container);
    expect(content).toContain(t.removed);
    expect(content).toContain('@brood_bot');
    expect(content).toContain(t.installation_bot);
    await unmount(page);
  });

  test('a poll that left before the unlink does not bring the linked chat back', async () => {
    server.profile = {
      ...installUnlinked(),
      state: 'linked',
      chat: { telegram_user_id: 4242, chat_id: 4242, username: 'alice_tg' },
    };
    const page = await mount();
    const stale = { ...server.profile };
    server.holdNextGet = true;
    let poll: Promise<void> | undefined;
    await act(async () => {
      poll = tickTelegramPolling();
    });
    await click(buttonNamed(page.container, t.unlink));
    await confirmDialog();
    await act(async () => {
      server.held?.(stale);
      await poll;
    });
    await settle();
    expect(text(page.container)).toContain(t.state.unlinked);
    expect(buttonNamed(page.container, t.link_issue)).toBeDefined();
    await unmount(page);
  });

  test('a poll or a header refresh during the code issue does not hide the issued code', async () => {
    const page = await mount();
    server.holdNextWrite = true;
    await act(async () => buttonNamed(page.container, t.link_issue)?.click());
    // Пока POST не ответил, сервер ещё без кода: чтение вернуло бы прежний
    // профиль новее записи, и код, показываемый один раз, пропал бы.
    await act(async () => {
      await Promise.all([tickTelegramPolling(), triggerHeaderRefresh()]);
    });
    await act(async () => server.releaseWrite?.());
    await settle();
    expect(
      page.container.querySelector(`a[href="https://t.me/brood_bot?start=${CODE}"]`)
    ).not.toBeNull();
    expect(text(page.container)).toContain(CODE);
    expect(text(page.container)).toContain(t.link_waiting);
    await unmount(page);
  });
});

describe('a write that failed after the server committed it', () => {
  test('a token save answered 500 shows the saved bot without a reload', async () => {
    const page = await mount();
    server.failAfterCommit = true;
    await type(tokenField(page.container), TOKEN);
    await submit(tokenForm(page.container));
    await settle();
    const content = text(page.container);
    expect(content).toContain(fill(t.refused, { reason: 'internal error' }));
    expect(content).toContain('@alice_dev_bot');
    expect(content).toContain(t.own_bot);
    await unmount(page);
  });

  test('a subscription change answered 500 shows the saved set without a reload', async () => {
    server.profile = { ...installUnlinked(), events: ['request_opened'] };
    const page = await mount();
    server.failAfterCommit = true;
    await flip(eventToggle(page.container, t.events.step_failed));
    await settle();
    expect(text(page.container)).toContain(fill(t.refused, { reason: 'internal error' }));
    expect(subscribed(page.container)).toEqual([t.events.request_opened, t.events.step_failed]);
    await unmount(page);
  });

  test('a code issue answered 500 shows that a code waits, without the code, and polls', async () => {
    const timers = spyOn(globalThis, 'setInterval');
    try {
      const page = await mount();
      server.failAfterCommit = true;
      await click(buttonNamed(page.container, t.link_issue));
      await settle();
      const content = text(page.container);
      expect(content).toContain(fill(t.refused, { reason: 'internal error' }));
      // Код сервер не показал (новый POST выдаст другой), но ждёт он его.
      expect(content).not.toContain(CODE);
      expect(content).toContain(t.link_pending_elsewhere.split('{{time}}')[0]);
      expect(content).toContain(t.state.waiting);
      expect(timers.mock.calls.filter(([, delay]) => delay === TELEGRAM_POLL_MS)).toHaveLength(1);
      await unmount(page);
    } finally {
      timers.mockRestore();
    }
  });
});

describe('states', () => {
  test('an error shows its reason and offers a new token or a new code', async () => {
    const reason = 'Telegram rejected the bot token (401): set a new bot token';
    server.profile = {
      ...installUnlinked(),
      bot_username: 'alice_dev_bot',
      own_bot: true,
      state: 'error',
      reason,
    };
    const page = await mount();
    const alerts = [...page.container.querySelectorAll('[role="alert"]')].map((node) => text(node));
    expect(alerts).toContain(fill(t.error_reason, { reason }));
    expect(text(page.container)).toContain(t.state.error);
    expect(buttonNamed(page.container, t.link_issue)).toBeDefined();
    expect(text(page.container)).toContain(fill(t.token_hint_own, { bot: '@alice_dev_bot' }));
    await unmount(page);
  });

  test('a stopped installation bot says why and gives no code', async () => {
    server.profile = { ...installUnlinked(), bot_reason: 'Telegram rejected the bot token (401)' };
    const page = await mount();
    const content = text(page.container);
    expect(content).toContain(
      fill(t.install_stopped, { reason: 'Telegram rejected the bot token (401)' })
    );
    expect(content).toContain(t.blocked_install_stopped);
    expect(buttonNamed(page.container, t.link_issue)).toBeUndefined();
    await unmount(page);
  });

  test('without any bot the page asks for a token first', async () => {
    server.profile = { ...installUnlinked(), bot_username: '' };
    const page = await mount();
    const content = text(page.container);
    expect(content).toContain(t.no_bot);
    expect(content).toContain(t.blocked_no_bot);
    expect(content).toContain(t.token_hint_none);
    expect(buttonNamed(page.container, t.link_issue)).toBeUndefined();
    await unmount(page);
  });

  test('subscriptions show every kind on or off, and every switch can be changed', async () => {
    server.profile = { ...installUnlinked(), events: ['request_opened', 'step_failed'] };
    const page = await mount();
    const toggles = eventToggles(page.container);
    expect(toggles).toHaveLength(TELEGRAM_EVENTS.length);
    expect(subscribed(page.container)).toEqual([t.events.request_opened, t.events.step_failed]);
    expect(toggles.filter((toggle) => toggle.disabled)).toEqual([]);
    expect(text(page.container)).toContain(t.events_hint);
    await unmount(page);
  });
});

describe('subscriptions', () => {
  test('a switch writes the full set and the page shows what the server saved', async () => {
    server.profile = { ...installUnlinked(), events: ['request_opened', 'step_failed'] };
    const page = await mount();
    await flip(eventToggle(page.container, t.events.pass_started));
    expect(writes()).toEqual([`PUT ${EVENTS_PATH}`]);
    expect(sentEvents()).toEqual([['request_opened', 'pass_started', 'step_failed']]);
    expect(subscribed(page.container)).toEqual([
      t.events.request_opened,
      t.events.pass_started,
      t.events.step_failed,
    ]);
    await flip(eventToggle(page.container, t.events.request_opened));
    expect(sentEvents().at(-1)).toEqual(['pass_started', 'step_failed']);
    expect(subscribed(page.container)).toEqual([t.events.pass_started, t.events.step_failed]);
    // Последний выключенный вид -- пустой набор, а не отказ панели.
    await flip(eventToggle(page.container, t.events.pass_started));
    await flip(eventToggle(page.container, t.events.step_failed));
    expect(sentEvents().at(-1)).toEqual([]);
    expect(subscribed(page.container)).toEqual([]);
    await unmount(page);
  });

  test('a kind of a newer server stays on: the page shows it and keeps it in the set', async () => {
    server.newerKinds = ['swarm_deleted'];
    server.profile = { ...installUnlinked(), events: ['request_opened', 'swarm_deleted'] };
    const page = await mount();
    // Выключенный, он пропал бы из списка, и панель не включила бы его снова.
    const newer = eventToggle(page.container, 'swarm_deleted');
    expect(newer.checked).toBe(true);
    expect(newer.disabled).toBe(true);
    await flip(eventToggle(page.container, t.events.step_failed));
    expect(sentEvents()).toEqual([['request_opened', 'step_failed', 'swarm_deleted']]);
    // Профиль ответа тоже несёт его: страница показывает его, как до записи.
    expect(subscribed(page.container)).toEqual([
      t.events.request_opened,
      t.events.step_failed,
      'swarm_deleted',
    ]);
    expect(eventToggle(page.container, 'swarm_deleted').disabled).toBe(true);
    await unmount(page);
  });

  test('a refusal is shown at the subscriptions and the switch stays as the server has it', async () => {
    server.profile = { ...installUnlinked(), events: ['request_opened'] };
    const page = await mount();
    // Отказ, который настоящий API даёт и известному виду (#295).
    const reason = 'session authorization changed';
    server.refusal = { status: 403, reason };
    await flip(eventToggle(page.container, t.events.pass_started));
    expect(writes()).toEqual([`PUT ${EVENTS_PATH}`]);
    const section = eventToggle(page.container, t.events.pass_started).closest('section')!;
    const alerts = [...section.querySelectorAll('[role="alert"]')].map((node) => text(node));
    expect(alerts).toEqual([fill(t.refused, { reason })]);
    expect(eventToggle(page.container, t.events.pass_started).checked).toBe(false);
    expect(subscribed(page.container)).toEqual([t.events.request_opened]);
    await unmount(page);
  });

  test('the switches wait for a write, a poll that left before it does not undo it, and a later one lands', async () => {
    server.profile = { ...installUnlinked(), events: ['request_opened'] };
    const page = await mount();
    const stale = { ...server.profile };
    server.holdNextGet = true;
    let poll: Promise<void> | undefined;
    await act(async () => {
      poll = tickTelegramPolling();
    });
    server.holdNextWrite = true;
    await flip(eventToggle(page.container, t.events.step_failed));
    // Пока запись не ответила, второй переключатель не шлёт набор поверх неё,
    // а переключатель показывает профиль сервера, а не щелчок.
    expect(eventToggles(page.container).every((toggle) => toggle.disabled)).toBe(true);
    expect(eventToggle(page.container, t.events.step_failed).checked).toBe(false);
    expect(subscribed(page.container)).toEqual([t.events.request_opened]);
    await act(async () => server.releaseWrite?.());
    await settle();
    await act(async () => {
      server.held?.(stale);
      await poll;
    });
    await settle();
    expect(subscribed(page.container)).toEqual([t.events.request_opened, t.events.step_failed]);
    expect(eventToggles(page.container).filter((toggle) => toggle.disabled)).toEqual([]);
    // Ответ записи лёг под своим номером: опрос, ушедший после неё, ложится
    // поверх и показывает то, что сервер сменил без этой страницы.
    server.profile = {
      ...server.profile,
      state: 'linked',
      chat: { telegram_user_id: 4242, chat_id: 4242, username: 'alice_tg' },
      events: ['pass_started'],
    };
    await act(async () => tickTelegramPolling());
    await settle();
    expect(text(page.container)).toContain(t.state.linked);
    expect(subscribed(page.container)).toEqual([t.events.pass_started]);
    await unmount(page);
  });

  test('the switch gets the focus back after its write, saved or refused', async () => {
    server.profile = { ...installUnlinked(), events: ['request_opened'] };
    const page = await mount();
    // Space на переключателе в фокусе: на время записи он выключен, и браузер
    // уводит фокус на body. Без возврата второй Space листал бы страницу.
    const saved = eventToggle(page.container, t.events.step_failed);
    saved.focus();
    server.holdNextWrite = true;
    await flip(saved);
    expect(saved.disabled).toBe(true);
    loseFocus();
    expect(focused()).toBe('BODY');
    await act(async () => server.releaseWrite?.());
    await settle();
    expect(subscribed(page.container)).toEqual([t.events.request_opened, t.events.step_failed]);
    expect(focused()).toBe(t.events.step_failed);
    // Отказ поддельный сервер отдаёт сразу: фокус уходит в том же щелчке.
    server.refusal = { status: 403, reason: 'session authorization changed' };
    const refused = eventToggle(page.container, t.events.pass_started);
    refused.focus();
    await act(async () => {
      refused.click();
      loseFocus();
    });
    await settle();
    expect(text(page.container)).toContain(
      fill(t.refused, { reason: 'session authorization changed' })
    );
    expect(refused.checked).toBe(false);
    expect(focused()).toBe(t.events.pass_started);
    await unmount(page);
  });

  test('a switch does not take back the focus the user moved during its write', async () => {
    const elsewhere = document.createElement('button');
    elsewhere.textContent = 'elsewhere';
    document.body.appendChild(elsewhere);
    const page = await mount();
    const toggle = eventToggle(page.container, t.events.step_failed);
    toggle.focus();
    server.holdNextWrite = true;
    await flip(toggle);
    elsewhere.focus();
    await act(async () => server.releaseWrite?.());
    await settle();
    expect(toggle.checked).toBe(false);
    expect(focused()).toBe('BUTTON elsewhere');
    await unmount(page);
    elsewhere.remove();
  });
});

describe('unsaved token', () => {
  test('leaving with a typed token asks first; staying keeps the page', async () => {
    const page = await mount();
    await type(tokenField(page.container), TOKEN);
    await act(async () => {
      await page.router.navigate('/swarms');
    });
    const confirmation = useNotificationStore.getState().confirmation;
    expect(confirmation.isOpen).toBe(true);
    expect(confirmation.options?.message).toBe(t.unsaved_message);
    await act(async () => confirmation.options?.onCancel?.());
    expect(page.router.state.location.pathname).toBe(PROFILE_PATH);
    expect(tokenField(page.container).value).toBe(TOKEN);
    await unmount(page);
  });

  test('after a save nothing is left to lose', async () => {
    const page = await mount();
    await type(tokenField(page.container), TOKEN);
    await submit(tokenForm(page.container));
    await act(async () => {
      await page.router.navigate('/swarms');
    });
    expect(useNotificationStore.getState().confirmation.isOpen).toBe(false);
    expect(page.router.state.location.pathname).toBe('/swarms');
    await unmount(page);
  });
});
