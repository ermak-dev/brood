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
- сущности Substrate (раздел «Сущности Substrate» ниже): `src/features/substrate/`
  (новый), `src/services/brood/http.ts` и `substrate.ts` (новые; `session.ts`
  берёт из `http.ts` общий `csrfToken`), иконки `IconSidebar*` в
  `src/components/ui/icons.tsx`, пункты nav-группы operate и переключатель
  проекта в шапке (`MainLayout.tsx` подключает `ProjectSwitcher`, его кнопка
  с подписью -- в `src/styles/layout.scss`), store
  `src/stores/useProjectStore.ts`, маршруты в `MainRoutes.tsx`, строки
  `nav.*`, `nav_meta.*`, `substrate.*`, `projects.*`, `actors.*`,
  `templates.*`, `workers.*`, `policy.*` в локалях, `tests/substrateDraft.test.ts`
  (новый).

## Сущности Substrate

Проекты (Atespace), акторы, шаблоны, воркеры и политика доступа -- свои
пункты меню; акторы и шаблоны -- в активном проекте из переключателя шапки.
Сервер brood (`brood/server/projects`) -- тонкий прокси к ateapi: объекты
ходят в каноническом protojson ateapi (как `kubectl ate get -o yaml` и демо-
шаблоны Substrate), редакторы меняют знакомые поля на прочитанном объекте и
шлют остальное как есть. Что правится, решает ateapi, а не панель:

- проект -- только имя: создать и удалить (непустой ateapi не удалит);
- актор -- шаблон (лишь у приостановленного) и `workerSelector`; Update
  заменяет объект целиком, поэтому прочитанный актор едет обратно со своим
  `sourceTag`. Предусловие (`uid`/`version`) панель перечитывает перед
  сохранением: ateapi поднимает версию и на каждой смене состояния актора, так
  что прочитанная при открытии редактора устаревает; правленые поля побеждают;
- шаблон неизменяем: «правка» -- копия; форма покрывает контейнеры, тома-
  каталоги снапшота, ресурсы, размещение и снапшоты, вкладка JSON -- всё
  остальное. Образ -- только по digest;
- воркер -- зеркало пода, его пишет ate-controller: из панели -- только drain;
- пул воркеров -- отдельный глобальный раздел, не зависит от проекта. Создание,
  изменение числа реплик и ресурсов, удаление; ноль реплик останавливает пул.
  Готовность -- `status.readyReplicas` относительно `spec.replicas`, причины
  проблем с sandbox -- условия `status.sandboxClasses`. Уменьшение и удаление
  могут прервать акторов; это не замена suspend. PUT и DELETE используют
  прочитанные `metadata.uid` и `metadata.resourceVersion`, конфликт не
  обходится перечитыванием версии за спиной пользователя;
- политика -- одна на скоуп (`default`): заводится POST, меняется PUT с
  прочитанными `uid`/`version`, глобальная не удаляется. Её версию двигают
  только правки политики, поэтому конфликт (409) значит чужую правку: черновик
  остаётся, панель предупреждает и берёт свежее предусловие -- следующее
  «Сохранить» заменит чужое осознанно.

Актор, названный UID шаблона, -- золотой: его заводит ateapi для золотого
снапшота шаблона; панель подписывает его и не даёт действий.

Черновик шаблона -- чистые функции `templateDraft.ts`: JSON-вкладка
проверяет форму полей, которые читает форма, до перехода на неё и до
сохранения -- иначе кривой JSON ронял бы страницу.

Каркас страниц общий: `PageHeader`, `EntityTable` (на телефоне -- карточки),
`EditorSheet` (Sheet апстрима; спрашивает перед закрытием с несохранённым),
`KeyValueEditor`, `ChipsInput`, `Segmented`; загрузка -- `useResource`,
действия с уведомлением и подтверждением -- `useActions`.

Сервер вне кластера (разработчик) ходит в ateapi через port-forward:
`ATEAPI_ENDPOINT=127.0.0.1:<порт>` в `.env` при `kubectl port-forward -n
ate-system svc/api <порт>:443`, плюс `ATEAPI_TOKEN` (токен с audience
`api.ate-system.svc`) и `ATEAPI_CA` (ClusterTrustBundle signer'а
`servicedns.podcert.ate.dev/identity`); TLS по-прежнему сверяет имя
`api.ate-system.svc`.

### Kubernetes API пулов

WorkerPool -- Kubernetes CRD, не protojson ateapi. Маршруты Django:
`GET/POST /api/worker-pools`, `GET/PUT/DELETE /api/worker-pools/<name>`,
`GET /api/worker-pool-defaults` (разрешённый образ воркера). Коллекция --
`{"results": [...]}`, объекты -- Kubernetes JSON. На DELETE тело содержит
`{"metadata": {"uid": "...", "resourceVersion": "..."}}`.

`brood/server/projects/kubernetes.py` ходит по HTTPS с CA и ротируемым
ServiceAccount-токеном, с таймаутом и без редиректов; `pools.py` ограничивает
поля и переводит отказы в HTTP. Настройки: `KUBE_API_URL`, `KUBE_API_TOKEN`,
`KUBE_API_CA`, `WORKER_POOL_IMAGE`. Выкладка задаёт их сама. Токен Kubernetes
отдельный от токена ateapi, у них разные audience.

RBAC разрешает только workerpools namespace `brood`, без доступа к Secret,
Pod и настройкам кластера. Поскольку контроллер создаёт привилегированные
поды, REST дополнительно разрешает только установленный digest образа,
класс gvisor, реплики, CPU/память, метки и nodeSelector. Подмена ServiceAccount,
pod-аннотации и произвольные поля запрещены. Полный доступ к Kubernetes
пользователь панели не получает. Доступ к REST -- сессия суперпользователя
Django, изменяющие запросы -- с CSRF.

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
