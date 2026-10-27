# Deploy

Серверный runtime — Go-образ `deploy/Dockerfile.go`: статические `brood`,
`bro` и `brood-migrate`, CA bundle и приватный каталог `/data`. Финальный
`scratch`-образ не содержит Python, shell, frontend toolchain или PostgreSQL.
Исходная React/Bun-панель (`web`) собирается отдельной panel-стадией
и встраивается в `brood`. Отдельный nginx edge не нужен для раздачи панели;
явно настроенный CLIProxyAPI подключается через авторизованный Go BFF.

## Состояние перехода

Удаление исходников не удаляет живые Deployment/Service/PVC, старые
пользовательские БД, токены провайдеров или акторы. Публичная старая установка
продолжает работать до отдельного управляемого cutover. Старые LLM-proxy
маршруты не являются частью API Go-сервера и не восстанавливаются автоматически.

`bin/brood deploy server` намеренно отказывает до обращения к кластеру:
для публичного переключения ещё нужны проверенная миграция пользователей,
восстановительный доступ и Go-only TLS-вход. Флага обхода этого отказа нет.
Автоматическая production-выкладка не должна маскировать это как успех;
до завершения cutover deploy job запускается вручную.

## Подготовка приватного Go endpoint

```sh
bin/brood deploy go
```

Команда проверяет владение кластером, namespace и именованными объектами,
читает точные адреса Kubernetes API до первой сборки/записи, собирает образ
по digest и применяет только Go-ресурсы: Deployment/Service/NetworkPolicy
`brood-go`, PVC `brood-go-data`, Secrets `brood-go-env` и `brood-go-tls`.
Стратегия Recreate исключает две реплики с одной SQLite-базой. Root filesystem
read-only, UID 65532, capabilities сняты. Миграции схемы применяет сам сервер.

Нужны:

- существующий принадлежащий установке namespace `brood`;
- доверенная ServiceAccount `django` и namespace-scoped worker-pool RBAC;
- `BROOD_SESSION_SECRET` в `.env`, не менее 32 символов;
- сертификат и ключ `secrets/tls.crt`, `secrets/tls.key`;
- `WORKER_POOL_IMAGES`: JSON-карта digest-pinned образов с обязательным
  `gvisor` и необязательным `microvm`; либо один `WORKER_POOL_IMAGE` для gvisor.

Без явной карты можно использовать конфигурацию существующего принадлежащего
этой установке Go Deployment. Старые Django Secrets не читаются. При отсутствии
валидных образов команда отказывает, а не пересоздаёт пулы за пользователя.

Имя `django` осталось только как точная infrastructure identity:
`system:serviceaccount:brood:django` и соответствующий SPIFFE principal уже
разрешены установщиком Substrate. Это не зависимость от Python runtime.
Переименование требует отдельной миграции доверия, его нельзя делать
одновременно с удалением старого кода.

Projected том содержит ротируемый ateapi-токен, service CA и pod identity
certificate. Kubernetes token имеет свою audience. RBAC разрешает только
операции workerpools в namespace `brood`, не secrets/pods/cluster-wide доступ.
Backend дополнительно ограничивает поля и разрешённые образы WorkerPool.
NetworkPolicy разрешает DNS, ate-system:443 и точные адреса Kubernetes API,
а не произвольный egress на 443.

Service слушает HTTPS на 8443. Для операторской проверки используйте только
loopback port-forward и доверенный CA, не публикуйте его на `0.0.0.0` и не
отключайте TLS-проверку. Проверяются `/healthz` и `/api/auth/session`, затем
реальный login и полномочия. Это подготовка endpoint, не публичный cutover.

## Миграция и откат

Протокол и команды — `docs/GO.md`. Перед переключением:

1. Сохранить консистентную копию старой БД и список atespaces с UID.
2. Импортировать пользователей в отдельную SQLite-базу с управляемым сбросом;
   старые hashes/sessions не переносить.
3. Явно связать atespaces с владельцами и проектами по проверенным UID.
4. Создать отдельного recovery admin, проверить вход, сброс и включение
   импортированных учётных записей и права двух обычных пользователей.
5. Подготовить Go-only TLS-вход с WebSocket и исключением actor-view tickets
   из access logs; только затем переключать public traffic.
6. Остановку старых процессов выполнять отдельно после проверки. Старые БД,
   PVC, provider credentials и пользовательские акторы автоматически не удалять.

До cutover откат — остановить отдельный Go Deployment: старый вход и БД
не изменены. После будущего cutover сначала остановить Go-записи и сохранить
SQLite вместе с WAL, затем вернуть прежний вход. Новые рои требуют отдельной
инвентаризации; обратная миграция не обещается.

## CI и restricted SSH

Тесты выполняются существующим runner `builder` с тегом `docker`.
`.ci/test.sh` проверяет Python dev-инструменты; `.ci/go-test.sh` — Go, включая
кросс-сборку linux amd64/arm64 и архивы. Зелёные тесты не означают готовый
публичный cutover.

`.ci/deploy.sh` передаёт по SSH SHA и `git archive`, добавляя `.env` и TLS
файлы с правами 0600 из защищённых CI file variables:

| Переменная | Назначение |
|---|---|
| `BROOD_DEPLOY_HOST` | SSH destination установки |
| `BROOD_DEPLOY_SSH_KEY` | ограниченный deploy key |
| `BROOD_DEPLOY_KNOWN_HOSTS` | проверенный ключ хоста |
| `BROOD_DEPLOY_ENV` | настройки релиза |
| `BROOD_DEPLOY_TLS_CERT`, `BROOD_DEPLOY_TLS_KEY` | TLS установки |

Обновление — `bin/brood dev ci set --file VAR=PATH`, просмотр имён и флагов
без значений — `bin/brood dev ci vars`. Ни адреса, ни ключа в репозитории нет.
Runner на целевой машине не добавляется.

В `authorized_keys` ключ имеет `restrict,from=<builder>,command=brood-deploy`.
Принудительная команда принимает только SHA и архив, serializes релизы через
flock и выполняет preflight до Substrate-мутаций. Её копию в `/usr/local/bin`
обновляет `deploy/provision.sh`, а не обычный deploy. Старый установленный
wrapper нужно обновить перед возобновлением автоматической выкладки.

## Пулы и microVM

`bin/brood deploy microvm` остаётся отдельной операцией только для пустой
установки: проверяет владение, отсутствие акторов, KVM и закреплённые assets.
Не удаляет исходный WorkerPool; требует drain перед уменьшением его replicas.
Восстановленная панель содержит управление воркерами, drain и worker pools.
API и инструменты Substrate также доступны; UID/version-предусловия нельзя
обходить ни из одного интерфейса.

Подготовка Go endpoint не меняет ресурсы, реплики и образы существующих пулов.
Отсутствие microVM-ёмкости — блокер полного workbench e2e, а не разрешение
остановить действующий coding-актор, уменьшить его ресурсы или менять kernel
и runtime policy хоста. Guest deployment выполняется только внутри собственной
microVM с приватными Docker/kind и свежими секретами (`docs/WORKBENCH.md`).
