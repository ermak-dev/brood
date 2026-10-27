# GO

Go-часть brood: сервер `brood` и пользовательский клиент `bro` (эпик #27).
Каталог — `go/`, модуль `brood.dev/go` (go 1.27), `go.work` в корне
репозитория подключает его. Python остаётся в `dev` и guest bootstrap
workbench; правила CLAUDE.md о командлетах, диспетчере и Django касаются
Python-кода, здесь — свои.

Установка сервера и клиента не требует Python, venv и Go toolchain:
бинарники распространяются готовыми. Внутри акторов и на машинах
разработки всё собирается из этого модуля.

## Устройство

| Пакет | Назначение |
|---|---|
| `brood/store` | SQLite, embedded-миграции, типизированные запросы, транзакции |
| `brood/auth` | Argon2id, серверные сессии, CSRF, троттлинг входа |
| `brood/app` | HTTP API, прикладные операции, авторизация пользователей/проектов/роёв |
| `brood/substrate` | адаптер Substrate: atespaces, акторы, шаблоны, ошибки |
| `brood/kube` | адаптер Kubernetes: projected credentials, preconditions |
| `brood/proxy` | терминал, WebSocket/HTTP proxy к акторам с проверкой прав |
| `brood/clientapi` | общая логика клиента: сессия по server origin, CSRF, TLS/CA |
| `brood/build` | сборщик Broodfile: plan/export, Git и Docker/buildx как внешние команды |
| `brood` | общие помощники сервера и клиента |
| `cmd/brood` | серверный бинарник `brood` |
| `cmd/bro` | клиентский бинарник `bro` |

Границы владения пакетами на время параллельной разработки — таблица
в `go/CONTRACT.md`; там же зафиксирован API входа, общий для панели,
сервера и `bro`. Соглашения эпика — единственный источник решений:
`brood dev bug show 27`.

Имя серверного бинарника `brood` не конфликтует с `bin/brood` рабочей
копии: установленный бинарник лежит в `PATH` установки, dev-launcher
остаётся входом Python-инструментов до конца переезда.

## Сборка и тесты

Всё выполняется из `go/`, go.work в корне уже подключает модуль:

```sh
cd go
go build ./...
go test ./...
```

Бинарники: `go build ./cmd/brood` и `go build ./cmd/bro`. Локально
собранные `go/brood`, `go/bro` и coverage-файлы в git не попадают
(`.gitignore`).

## Зависимости

Политика — минимальный закрытый список; новая зависимость требует записи
здесь и причины. ORM, фреймворки и универсальные диспетчеры не вводятся
(решение эпика #27).

| Зависимость | Роль |
|---|---|
| `golang.org/x/crypto` | Argon2id для паролей |
| `modernc.org/sqlite` | SQLite без cgo, драйвер и WAL |
| `github.com/alexedwards/scs/v2` | серверные сессии в БД |
| `gopkg.in/yaml.v3` | разбор Broodfile |
| `go.ate.dev/substrate` | клиент Substrate (см. replace ниже) |

Остальное в `go.sum` — транзитивные зависимости этих пяти. HTTP —
`net/http`, SQL — явные запросы.

## Pinned Substrate

До публикации модуля Substrate `go.mod` держит локальный replace:

```
replace go.ate.dev/substrate => /home/ermak/substrate-4ebd668
```

Это worktree коммита `4ebd668d62e3f55753765a46579fc6deb81b2f7f`,
временная привязка, свойство машины автора, не обязательный путь.
После публикации достаточно удалить строку replace и дать
`go get go.ate.dev/substrate@<версия>` с тем же закреплённым коммитом
(или его опубликованным тегом); остальные строки `go.mod` не меняются.
Пока replace живёт, сборка на другой машине требует такого же worktree
по этому пути.
