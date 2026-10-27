# brood

Пул агентов на [Agent Substrate](https://github.com/agent-substrate/substrate).
Сервер `brood` и пользовательский клиент `bro` — самостоятельные Go-бинарники.
Исходная React/Bun-панель в `web` встроена в сервер и расширена страницами
пользователей, прав, проектов и роёв. Python остаётся для инструментов разработки,
управления локальным Substrate и bootstrap изолированного workbench.

## Разработка

Нужны Python 3.12+ с venv, Go из `go/go.mod`, Git и Docker с buildx.

```sh
bin/brood dev setup
bin/brood dev web test
make -C go build
bin/brood dev test
make -C go fmt-check vet test
venv/bin/ruff check
```

`bin/brood` активирует venv своей рабочей копии. `brood dev setup` добавляет
в `.env` случайный `BROOD_SESSION_SECRET`, не печатая его. Go-бинарники
читают настройки из окружения, не загружают `.env` сами. Для локального
сервера экспортируйте настройки из своего доверенного `.env`:

```sh
set -a
. ./.env
set +a
go/bin/brood bootstrap-admin
go/bin/brood serve
```

По умолчанию сервер доступен только на `http://127.0.0.1:8080`.
Внешний адрес требует явной настройки TLS; новая SQLite-база создаётся
с текущей схемой при открытии, legacy данные не импортируются. Управление пользователями, проектами, членством и своими роями —
в панели и API. Администратор имеет доступ ко всем роям; член проекта не
видит чужие рои автоматически.

В другом терминале:

```sh
go/bin/bro login http://127.0.0.1:8080
go/bin/bro whoami
go/bin/bro logout
go/bin/bro build development --registry localhost:5002 --atespace example
```

Сборка использует внешние Git и Docker/buildx и только committed-снимки.
Она выдаёт образ и манифесты, но не запускает акторов. Подробности —
[Build](docs/BUILD.md), [Go](docs/GO.md), [Web](docs/WEB.md).

## Инструменты разработчика

```sh
bin/brood dev bug list
bin/brood dev bug show <iid>
bin/brood dev bug new "<title>" --label type::bug --body-file body.md
bin/brood dev bug start <iid>
bin/brood dev ci
bin/brood dev ci why <id>
bin/brood dev ci log <job>
bin/brood dev ci vars
bin/brood substrate install
bin/brood substrate start
```

Координаты трекера берутся из git origin, переопределения и `GITLAB_TOKEN` —
из `.env`. Секреты не коммитятся. Полная справка — `bin/brood` без аргументов.
Локальный Substrate — [Substrate](docs/SUBSTRATE.md), workbench —
[Workbench](docs/WORKBENCH.md), deployment и границы перехода со старой
установки — [Deploy](docs/DEPLOY.md). Правила разработки — `CLAUDE.md`.

Публичный `bin/brood deploy server` создаёт чистое приложение и нового
администратора из защищённых настроек релиза, проверяет вход и только затем
убирает старые Django/Postgres ресурсы. Переноса старых данных нет. Общий
Substrate, worker pools, provider credentials и пользовательские акторы
не пересоздаются.
