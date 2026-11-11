# MCP

## Один каталог команд

Каждый инструмент — команда пользовательского Go-клиента `bro`, а не отдельная
реализация внутри MCP. Пакет `go/brood/command` содержит декларации команд:
путь CLI, имя инструмента, описание, аргументы, значения по умолчанию,
ограничения и annotations. Например, `bro session history` соответствует
`session_history`, `bro project limit` — `project_limit`.

`ParseCLI` и `ParseTool` возвращают один `Invocation`. CLI вызывает executor
непосредственно; `bro mcp` строит `tools/list` из тех же деклараций и вызывает
тот же executor. MCP не запускает командлеты подпроцессами, не разбирает
терминальный вывод и не имеет собственной реализации операций.

```text
bro <command> ───────┐
                    ├─ command.Invocation ─ executor ─ авторизованный сервис
bro mcp ─ metadata ─┘
```

Схемы отвергают неизвестные и повторяющиеся поля, null, неправильные типы и
выход за границы. Каноническое представление тоже ограничено по размеру.
Metadata описывает возможности, но не выдаёт полномочий: сервер проверяет их
при каждом вызове и повторно перед выдачей приватных результатов.

## Два режима исполнения

Оператор использует сохранённую HTTPS-сессию `bro login`; TLS, Origin и CSRF
обслуживает существующий `clientapi`. Рой выбирается глобальным `--swarm ID`
или `BROOD_SWARM`. Команды установки — проекты, пулы, диагностика — работают
также без выбранного роя. `bro diagnose` читает только локальные метаданные,
не создаёт каталог состояния и не обращается к серверу.

```sh
bro --swarm SWARM_ID agents
bro --swarm SWARM_ID tail worker --lines 40
bro models
bro node
bro project limit --name brood --actors 3
bro mcp
```

Гость выбирает режим явно: `bro --guest ...` или
`BROOD_EXECUTION_MODE=guest`. При ошибке broker нет отката на операторскую
сессию. В гостевом режиме login/logout/whoami, проекты и push не используют
операторский транспорт.

Профиль `claude-proxmox` добавляет следующий сервер к Claude Code:

```json
{"mcpServers":{"brood":{"command":"/opt/brood/bin/bro","args":["--guest","mcp"]}}}
```

Это дополнительный `--mcp-config`, не `--strict-mcp-config`. Пользовательские
MCP-настройки сохраняются. Namespace инструментов — `mcp__brood__*`, имя
сервера — `brood`. Python и NATS в госте не нужны. `brood-guest` остаётся
доверенным supervisor, а гостевой образ дополнительно содержит `bro`.

## Каталог и адаптация mop

Источник сравнения — `~/mop` на commit `e8f04f26`: 25 инструментов.
Brood добавляет `start`, `reset`, `pipeline_visit_complete` и `ask_operator`:
у оператора 29 инструментов, в гостевом MCP — 23. Перенос не подключает Brood к старой установке mop. Nomad job,
узел, контейнер PVE, пул Brood и native Worker — разные сущности.

| Команда bro | Инструмент | Контракт Brood |
|---|---|---|
| `agents` | `agents` | Managed-акторы выбранного роя; running не означает idle или чистый Git |
| `send` | `send` | Сообщение и квитанция доставки, не ответ модели |
| `ask operator` | `ask_operator` | Вопрос актора оператору своего роя (#210); ответ приходит сообщением оператора и граф не продвигает. Выполняет только гостевой путь |
| `pipeline visit complete` | `pipeline_visit_complete` | Исход шага актора (`success`/`fail`) и необязательный `report` (#233): что сделано и где результат, до 16384 байт текста без управляющих символов, кроме перевода строки и табуляции. Отчёт хранится на шаге один раз и показывается оператору в узловом запросе следом. Выполняет только гостевой путь |
| `dispatch` | `dispatch` | Явная аренда allocation с request ID; send/show/release |
| `tail` | `tail` | Ограниченный снимок headless VT состояния PTY |
| `slash` | `slash` | Белый список команд в доказанную сессию |
| `session history` | `session_history` | Приватная история с cursor и epoch |
| `session terminal` | `session_terminal` | Приватное ro/rw подключение со снимками экрана и последовательностями |
| `add` | `add` | Master создаёт worker в своём одобренном пуле и рое |
| `add --origin URL` | `add` (`origin`) | HTTPS Git origin нового worker; без флага — origin master, затем repository проекта ([PROXMOX.md](PROXMOX.md)) |
| `delete` | `delete` | Асинхронное удаление CT и диска; требуется confirm |
| `stop` | `stop` | Остановка managed CT с сохранением диска и квоты |
| `start` | `start` | Запуск остановленного CT с прежним диском и поколением; работающий не меняется. Аналог mop `update`, поднимающего остановленный папет |
| `restart` | `restart` | Перезапуск Claude в bypass-режиме внутри прежнего CT; требуется confirm |
| `update` | `update` | Явный отказ: безопасное обновление immutable-профиля ещё не реализовано (#133); остановленный актор поднимает `start` |
| `recycle` | `recycle` | Очистка Git-клона и новая Claude-сессия в прежнем CT; требуется confirm |
| `wipe` | `wipe` | Очистка тихого Git-workspace без удаления клона; требуется confirm |
| `reset` | `reset` | Новый CT и поколение: workspace удаляется, при заданном origin клонируется заново; требуется confirm |
| `project add` | `project_add` | Создание проекта Brood с описанием, не Git checkout или сборка образа |
| `project limit` | `project_limit` | Чтение или атомарное изменение только max_actors |
| `landing` | `landing` | Advisory-токен интеграции проекта: show/take/give |
| `node` | `node` | Inventory с явными provider и kind |
| `node drain` | `node_drain` | Managed admission closure или native Worker drain, не обещание эвакуации |
| `node up` | `node_up` | Только повторное включение managed-пула; native undrain отвергается |
| `models` | `models` | Каталог установленного CLIProxyAPI без раскрытия ключа |
| `profile` | `profile` | Подлинная идентичность, роль и область вызова |
| `stat` | `stat` | Ограниченный transcript-backed usage выбранного актора; частичный результат отмечен |
| `diagnose` | `diagnose` | Локальная диагностика оператора или собственные факты гостя |
| `server diagnose` | `server_diagnose` | Удалённая redacted-диагностика runtime для администратора |

`project_*`, `node*` и `server_diagnose` не объявляются в гостевом MCP.
Просмотр своих сессий доступен worker; просмотр соседей и управляющие
действия требуют действующей master-делегации. `start` и `reset` master
выполняет только для своих workers — созданных им через `add`; как mop
`update`, чужой папет — отказ. Оператору нужно право управлять роем, как
для stop/delete. Наличие инструмента в `tools/list` не является
разрешением его выполнить.

Name-only `add` наследует одобренный пул именно от managed master, а origin —
от master или, если у master он пуст, repository проекта. `--origin`
канонизируется сервером: допустим только repository проекта с серверным
Git-доступом (активный проектный Git credential или `BROOD_GIT_REMOTES`,
через серверный шлюз Git) или публичный host allowlist гостя; иное — отказ
без резервирования. Оператор
не может неявно выбрать пул через эту команду: создание с явным шаблоном
остаётся в существующем API/панели. Native-акторы не изображаются managed
акторами в этом канале. Управление занятой immutable-конфигурацией и полный
workspace-preserving reboot CT — отдельные задачи #133 и #130.

`restart` не меняет CTID, образ, PVE ownership и relay generation. Новая
сессия запускается тем же launch, что и автостарт при загрузке гостя:
`claude --dangerously-skip-permissions` с сервером `brood`, без диалогов
onboarding, trust и bypass ([PROXMOX.md](PROXMOX.md), «Автономный запуск
Claude»). Модель новой сессии выполняет инструменты без запросов
разрешений; то же относится к `recycle`. Для `wipe` и `recycle` нужен
проверенный Git-клон строго в `/workspace/project`: неизвестный Git state,
неподходящая конфигурация, symlink/gitdir вне корня дают отказ до остановки
сессии и очистки. `restart` workspace не чистит и Git-клона не требует (#158):
актору без origin хватает каталога `/workspace/project` без symlink.
Проверенный клон, найденный до остановки, `restart` проверяет снова перед
запуском, и потеря клона или каталога за время остановки даёт unknown.
Несовпадающая PTY identity даёт отказ до остановки сессии; живые процессы в
workspace дают отказ `wipe`, а у `restart` и `recycle` после остановки дают
unknown. Эти команды не загружают произвольный origin, bootstrap или shell из
аргументов.
`restart` и `recycle` ждут освобождения workspace до 4 секунд и нового session
proof до 8 секунд в общем бюджете helper 15 секунд; timeout или отмена после
эффекта дают unknown без автоматического освобождения reservation.
`wipe` требует остановленной Claude-сессии, а не выключенного CT.
Выключенный CT возобновляет `start`.

`start` и `reset` — те же durable intents, что действия resume и reset
API/панели: принятие — записанное намерение, а не готовность гостя.
`start` сохраняет CTID, диск, owner nonce и поколение; для работающего
актора это no-op без нового намерения и эффекта провайдера. `reset`
останавливает и удаляет прежний CT, отзывает guest token старого поколения
с первого шага и только после подтверждённого отсутствия создаёт CT с тем
же CTID, новым owner nonce и поколением +1. Workspace не переносится: при
заданном origin актора гость клонирует его заново (#146, #147), иначе
workspace пуст. Координация с участием актора и workspace reservation
дают отказ обоим до любого эффекта провайдера.

## Доставка и координация

`send(to, message, wait_seconds=0)` адресует другого managed-актора того же
роя. Отправителя задаёт сервер. Максимальное ожидание — 300 секунд; это
ожидание квитанции/idle, а не текста ответа. Без ожидания простоя гость
адресата ждёт квитанцию доставки до 15 секунд ([PROXMOX.md](PROXMOX.md),
«Приём сообщений в госте»). Ответ актора — новый `send`.

Только совпавшая квитанция подтверждает доставку. `submitted; delivery
unconfirmed` не означает ни успех, ни гарантированную потерю. Повтор может
создать дубликат. Idle не доказывает доставку или выполнение задания.
Операторское сообщение помечается `operator:<username>` и не выдаётся за
адрес актора; обратного операторского inbox в этой реализации нет.

`dispatch` требует `to`, сообщение и `request_id`. Сервер сначала атомарно
занимает allocation, затем отправляет задачу. Request ID связан с исходным
текстом; повтор не отправляет его заново и не оживляет освобождённую аренду.
Неизвестная доставка сохраняет аренду. Она не доказывает чистоту клона и не
передаёт Git credentials человека.

```sh
bro --guest dispatch worker "Run the project tests" --request-id task-1
bro --guest dispatch worker --request-id task-1 --action show
bro --guest dispatch worker --request-id task-1 --action release
bro --guest landing --action take --request-id integration-1
bro --guest landing --action give --request-id integration-1
```

Аренды и landing хранятся в SQLite, связаны с UID/поколением, человеком и
проектом/роем. У них нет автоматического force-steal или expiry. Перед
остановкой, запуском, удалением, reset и очисткой workspace активную
координацию нужно явно освободить. Администратор может выполнить точное
восстановительное release/give по request ID; это не impersonation master
и не новая отправка.
Human-оператор не может выполнять dispatch send или landing take без
managed actor identity.

Workspace mutations атомарно занимают отдельную reservation в том же SQLite
journal. Она исключает новый dispatch, landing и конфликтующий lifecycle на
всё время RPC. Подтверждённое завершение освобождает reservation; неизвестный
исход сохраняет её и сообщает operation ID. После exclusive restart сервера
незаконченные reservation становятся unconfirmed, но не освобождаются.
Администратор сначала проверяет workspace и затем может выполнить явный
`dispatch --action release` с точным operation ID. Inflight reservation
освобождать нельзя. Это восстановление разрешения на последующую работу,
не доказательство того, что предыдущий эффект не произошёл.

Landing — advisory-токен всего проекта, не разрешение GitLab merge/push.
Проверка SQLite и внешнее RPC не являются одной распределённой транзакцией.
Неизвестные исходы требуют сверки, а не автоматического повторения.

## Приватная сессия

Сервер разрешает target по имени только внутри роя, сохраняет UID и
поколение и передаёт caller identity сам. Session RPC идёт по отдельному
ограниченному SSH-каналу `brood-session-rpc-v1`, а не через произвольный exec,
URL или actor-owned socket path.

Root запускает фиксированный helper UID/GID 1000 с NoNewPrivs, очищенными
capabilities и фиксированным окружением. Только helper открывает реестр и
сокеты Claude. Он проверяет peer PID/UID, process-start identity, CWD,
config home, foreground TTY и точный PID/start/epoch текущего PTY child. История читается из фиксированного корня
без symlink traversal; неполная строка остаётся pending, слишком большая
или неверная запись даёт notice. История ограничена 8 MiB prefix scan и
явно сообщает partial/reset.

`session_terminal` — screen-snapshot, не lossless PTY stream. `open` выдаёт
случайный caller-bound `tid`; mode по умолчанию ro, writer один. Последующие
операции используют возрастающий `seq`, начиная с 1. Последний одинаковый
повтор возвращает прежнюю квитанцию без повторного ввода; изменённый payload
с тем же seq отвергается. Неизвестная запись отключает writer. Cursor —
непрозрачная строка снимка, `read --wait` ограничен 15 секундами, `data` —
буквальный UTF-8 ввод до 4096 байт, не base64. `close` отсоединяет только
подключение, не останавливает Claude или PTY holder. Disconnect уничтожает metadata.

Максимум четыре подключения, idle TTL 60 секунд, writer TTL 30 секунд.
Slash и workspace mutations используют тот же writer gate. Белый список
slash: `/model`, `/clear`, `/compact`, `/rc`, `/status`, `Escape`; текст с
управляющими символами или произвольными аргументами не превращается в shell.
Ввод `/model` не является доказательством выбора конкретной модели.

`stat` читает bounded project/subagent transcripts, дедуплицирует usage по
message identity и не приписывает затраты человеку по имени актора. Это не
вечный биллинг: удалённые transcripts недоступны, неполные сканы отмечаются.

## Граница полномочий и протокол

```text
Claude -> bro --guest mcp (UID 1000)
       -> /run/brood/swarm.sock (root:1000, 0660)
       -> supervisor -> аутентифицированный relay
       -> сервер: текущие identity/generation/swarm/delegation
       -> relay получателя -> fixed helper UID 1000 -> Claude/PTY/transcripts
```

Broker проверяет SO_PEERCRED, держит exclusive lock в root-owned каталоге и
не принимает credentials/UID/endpoint от модели. Старый caller после
reconnect не используется. Legacy agents/add/send envelopes поддерживаются
при переходе; canonical arguments с legacy полями не смешиваются. Запросы
и ответы relay ограничены 128 KiB; одновременно не более восьми RPC/IPC.
Машинные credentials остаются у root supervisor. Ключ CLIProxyAPI остаётся
на сервере; helper использует тот же fixed-loopback gateway, что исходная
Claude-сессия. Ttyd и Go PTY holder живут вне клона; только app wrapper
выбирает `/workspace/project`. Browser writer владеет вводом и размером PTY;
пока он подключён, конфликтующие input/resize и restart/recycle с остановкой
сессии получают отказ, без принудительного захвата. Это не меняет семантику
явных lifecycle stop/reset/delete всей CT. Чтение снимков и transcript доступно по прежним
правам. Приватный holder-сокет root-owned и не выбирается аргументами модели.

MCP tools-only `2025-03-26`: initialize, notifications/initialized, ping,
tools/list, tools/call. Ошибка возвращается с причиной и `isError: true`;
неподтверждённый исход не становится успешным выполнением команды. CLI
выходит ненулевым кодом при отказе/неопределённости. Долговечное принятие
lifecycle intent не объявляется завершённой очисткой или readiness.

Проверки используют временные SQLite/Git/transcript fixtures, настоящий
stdio-бинарник, Unix sockets и изолированный TLS/WSS/SSH relay. Они не
являются доказательством выкладки нового образа, живого PVE lifecycle или
успешной платной модельной задачи. Пользовательские master/worker не
перезапускаются для приёмки этой разработки.
