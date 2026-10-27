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

## Соглашения

- Ошибки: `fmt.Errorf`/ошибки-значения; отказы CLI — строкой, без паник.
- Пароли: Argon2id (golang.org/x/crypto/argon2), параметры в кодировке хэша.
- ID: строковые ULID/UUID; `atespace_name = "swarm-" + <id без дефисов, lower>`.
- SQL: явные запросы, встроенная текущая схема для новой базы, без миграций
  и импорта старых данных; SQLite WAL (modernc.org/sqlite, без cgo).
  Повторное открытие текущей схемы идемпотентно, несовместимая схема — отказ.
- Время: UTC; парсинг/печать RFC3339.
