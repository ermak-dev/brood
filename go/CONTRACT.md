# Go-контракт эпика #27

Общий модуль `brood.dev/go` (go 1.27, go.work в корне репозитория).
Единственный источник правил — эпик #27 (`bin/brood dev bug show 27`).
Pinned Substrate: публичная версия go.ate.dev/substrate v0.6.1-0.20261023032142-4ebd668d62e3, без машинных replace.

## Владение файлами (параллельная работа)

| Пакет | Владелец (issue) |
|---|---|
| `go/brood/store/**` | #29 |
| `go/brood/auth/**` | #30 |
| `go/brood/app/**` | #31 |
| `go/brood/substrate/**`, `go/brood/kube/**` | #32 |
| `go/brood/proxy/**` | #33 |
| `go/cmd/bro/**`, `go/brood/clientapi/**` | #34 |
| `go/brood/build/**` | #35 |
| `CLAUDE.md`, `.gitignore`, `docs/GO.md` | #28 |

Никто не редактирует чужие пакеты, `go.mod`, `go.work` и `go.sum`:
недостающие зависимости перечислить в отчёте, их добавляет владелец репозитория.
Тесты — `go test ./...`; библиотеки молчат, печатают только cmd.

## API входа (общий для панели, сервера и bro)

- Cookie: сессия — `session`, CSRF — `csrf`. Обе HttpOnly, кроме `csrf`
  (панель читает значение), Secure при HTTPS, SameSite=Lax.
- Заголовок изменения: `X-CSRFToken: <значение csrf cookie>`.
- `GET /api/auth/session`
  - анонимно: `200 {"authenticated": false}` + установка csrf cookie;
  - после входа: `200 {"authenticated": true, "user": {"username", "display_name", "is_admin"}}`.
- `POST /api/auth/login` `{"username", "password"}` + X-CSRFToken
  - успех: `204`, новая `session` cookie, ротация csrf;
  - отказ: `401 {"detail": "invalid credentials"}` (без раскрытия, что именно неверно).
- `POST /api/auth/logout` + X-CSRFToken: `204`, сервер удаляет сессию, cookie чистятся.
- Ошибки — JSON `{"detail": "<английское сообщение>"}`; без трассировок и секретов.

## Управление пользователями

Только активный администратор:

- `GET /api/admin/users/{username}` — username, display_name, is_admin, disabled;
- `PUT /api/admin/users/{username}` — все три редактируемых свойства
  `display_name`, `is_admin`, `disabled` обязательны; изменение атомарное,
  успех 204. Username неизменяем. Изменение роли/блокировки отзывает сессии;
- `DELETE /api/admin/users/{username}` — 204 только если у пользователя нет
  нетерминальных роёв; иначе 409 с требованием сначала передать или удалить их.

Последнего активного администратора нельзя заблокировать, понизить или
удалить: 409 без частичного сохранения. Аноним получает 401, обычный
пользователь 403; изменяющие методы требуют Origin/CSRF.

## Владелец роя

Ответы создания, чтения и списка роёв содержат `owner_id` и
`owner_username`: неизменяемый идентификатор и текущее имя пользователя.
Имя раскрывается только после обычной проверки доступа к рою; членство
в проекте не открывает владельцев чужих роёв.

## Сборка и push

`bro build [target]` читает текущий `./Broodfile`, не требует login/registry и
сохраняет OCI-архив с JSON-артефактами атомарно. `--project NAME`/`BUILD_PROJECT`
задаёт логическое назначение, не путь. `--ref`, `--registry`, `--atespace` удалены.
Справка опций — `bro build --help`, не корневая справка.

`bro push [target] --project NAME` использует текущую bro login сессию. Назначение
разрешается в доступный активный swarm; сервер всегда назначает atespace сам.
Base API: `/api/swarms/{id}/builds/{environment}`:

- `HEAD /blobs/{sha256}` — 200/404, только свой repository.
- `POST /uploads` — 201 `{upload_id}`, upload связан с точной сессией, TTL 30 минут.
- `PATCH /uploads/{upload_id}` — octet-stream, Content-Length, chunk до 32 MiB; 204.
- `PUT /uploads/{upload_id}?digest=sha256:...` — finalization, 204.
- `PUT /manifests/{sha256}` — OCI manifest/index до 4 MiB, проверка digest и
  связанных дескрипторов в своём repository; 200 `{image}`.
- `POST /templates` — `{image, actor_template, egress_policy}`; 200
  `{image, template, project}`. Image/atespace server-scoped, идентичный retry
  идемпотентен; другое содержимое существующего имени — 409. Акторы не создаются.

Все запросы используют обычные session/CSRF и owner/membership/admin проверки.
Публичного `/v2/`, Docker credentials и отдельного push bearer нет. Registry URL
остаётся серверной настройкой. Потоки ограничены размером, временем и количеством
upload state; приватные Location/registry errors не передаются клиенту.
Template-only push нормализует unspecified golden mode в DISABLED и отказывает
explicit ENABLED. Runtime EgressPolicy не преобразуется в preview golden policy:
`egress_policy_deferred: true` и CLI-сообщение явно показывают, что правила из
локального egress-policy.json нужно применить при будущем создании actor.

## Соглашения

- Ошибки: `fmt.Errorf`/ошибки-значения; отказы CLI — строкой, без паник.
- Пароли: Argon2id (golang.org/x/crypto/argon2), параметры в кодировке хэша.
- ID: строковые ULID/UUID; `atespace_name = "swarm-" + <id без дефисов, lower>`.
- SQL: явные запросы, встроенная текущая схема для новой базы, без миграций
  и импорта старых данных; SQLite WAL (modernc.org/sqlite, без cgo).
  Повторное открытие текущей схемы идемпотентно, несовместимая схема — отказ.
- Время: UTC; парсинг/печать RFC3339.
