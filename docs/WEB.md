# WEB

Веб-интерфейс brood -- форк панели CLIProxyAPI
([Cli-Proxy-API-Management-Center](https://github.com/router-for-me/Cli-Proxy-API-Management-Center),
MIT) в `brood/server/web`. Сейчас он управляет LLM-прокси установки; страницы
самого brood добавляются в него же, поверх API Django. На стенде он отдаётся с
корня входа: `https://brood.corp.ermak.dev/`.

## Устройство

React 19, TypeScript, Vite; менеджер пакетов -- Bun (`packageManager` в
`package.json`). Сборка -- один файл `dist/index.html` (vite-plugin-singlefile),
маршруты -- после `#`, так что серверу нужен только корень.

Внутри `brood/server/web` действуют соглашения апстрима (их `AGENTS.md`:
Bun, стиль, i18n во всех шести локалях, тесты в `tests/` на `bun:test`):
так форк дешевле сливать с апстримом. Правила brood (`CLAUDE.md`) -- за его
пределами.

## Вход

У апстрима вход -- management-ключ прокси, который вводят в браузере и
хранят в localStorage. В brood ключа в браузере нет:

- панель входит в сессию Django (`brood/server/base/views.py`):
  `GET /auth/session` -- кто вошёл и кука `csrftoken`, `POST /auth/login`,
  `POST /auth/logout` с заголовком `X-CSRFToken`;
- запросы панели в `/v8/management/` идут без `Authorization`; вход (nginx)
  спрашивает Django `/auth/llm` (`auth_request`) и при 2xx сам ставит
  `Authorization: Bearer <CLIPROXY_MANAGEMENT_KEY>`, затирая клиентский;
- `/auth/llm` пускает суперпользователя; небезопасный метод -- только с
  `Origin` этого же сайта: сессия едет кукой, и чужая страница иначе могла
  бы дёрнуть management от имени вошедшего;
- 401 ворот панель понимает как выход и показывает форму входа.

## Брендинг

Панель называется Brood: заголовки, сокращение в боковой панели, слово на
странице входа, `<title>`. Логотип -- `docs/logo.png` (тёмный знак на
прозрачном фоне); в панели он -- плитка 256 px, знак на белом скруглённом
квадрате, чтобы читаться и в тёмной теме, и во вкладке браузера:
`src/assets/logoInline.ts` (`INLINE_LOGO`, ею же `main.tsx` ставит favicon) и
плитка 64 px favicon'ом в `index.html`. Новый логотип -- новые плитки из
`docs/logo.png` (Pillow: знак по альфа-каналу, 80% стороны, радиус 20%).

Страница входа не показывает адрес бэкенда и подзаголовок про версию
CLIProxyAPI: бэкенд всегда этот же сайт. Спонсорского «Быстрого старта»
(APIKEY.FUN) нет: ни пункта меню, ни страницы, `/quick-start` уводит в
AI Providers.

## Правки форка

Против апстрима правлены:

- вход: `src/services/brood/session.ts` (новый), `src/stores/useAuthStore.ts`,
  `src/pages/LoginPage.tsx`, `src/types/auth.ts`, строки `login.*` в
  локалях, `tests/broodSession.test.ts` вместо
  `tests/legacyBackendProbe.test.ts` (вход по ключу с пробой `/v0` в форке
  не существует);
- брендинг: `index.html`, `src/main.tsx`, `src/assets/logoInline.ts`,
  `src/pages/LoginPage.module.scss`, логотип в `LoginPage`, `SystemPage` и
  `MainLayout`, строки `title.*`, `splash.title`, `sidebar.subtitle`,
  `system_info.about_title` в локалях;
- без быстрого старта: `src/router/MainRoutes.tsx`,
  `src/components/layout/MainLayout.tsx`;
- проекты (atespace Substrate за `/api/projects` сервера brood, акторы
  проекта за `/api/projects/<name>/actors`): `src/features/projects/` (новый),
  `src/services/brood/projects.ts` (новый), `IconSidebarProjects` в
  `src/components/ui/icons.tsx`, пункт в nav-группе operate `MainLayout.tsx`,
  маршруты в `MainRoutes.tsx`, строки `nav.projects`, `nav_meta.projects`,
  `projects.*` в локалях.

## Сборка и проверки

```sh
brood dev web build    # brood/server/web/dist/index.html
brood dev web test     # bun run verify: тесты, eslint, сборка
```

Bun идёт в контейнере того же образа, которым панель собирает образ входа
(`brood/dev/deploy/edge/Dockerfile`, стадия `panel`); на хосте нужен только
Docker. `dist/` в git не едет: образ входа собирает панель из исходников.

## Обновление с апстрима

Форк -- `git subtree` с `--squash`:

```sh
git subtree pull --prefix=brood/server/web \
    https://github.com/router-for-me/Cli-Proxy-API-Management-Center.git vX.Y.Z --squash
```

Затем -- конфликты в файлах из «Правок форка», `brood dev web test`,
версия в `VERSION=` стадии `panel` образа входа и, если панель требует
нового прокси, образ CLIProxyAPI в `brood/dev/deploy/manifests/cliproxy.yaml`.
Сейчас: панель v1.26.7, CLIProxyAPI v8.0.41.
