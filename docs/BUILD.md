# Build

Broodfile описывает серверный проект, сборочные входы и шаблоны. Версия первой схемы —
`brood.dev/v1alpha1`; строгий parser/validator — `go/brood/build/plan.go`. Backend первой
версии — Dockerfile/BuildKit. Bootstrap внутри актора не конфигурирует runtime
Substrate и не выполняется при сборке.

## Декларация проекта

Верхний `name` задаёт имя проекта по умолчанию; `--project NAME` или
`BUILD_PROJECT` меняет имя назначения. Дополнительные поля задаются в
необязательном блоке `project`:

```yaml
name: my-project
project:
  description: Coding workspace
  # Если repository не задан, используется Git origin текущего checkout.
  repository: https://git.example.com/team/my-project.git
  capacity:
    worker_selector: {brood.dev/purpose: development}
    max_swarms: '4'
    max_actors: '12'
    max_cpu_millicores: '16000'
    max_memory_bytes: '34359738368'
```

Repository разрешает клиент из текущей рабочей копии, не из checkout сервера
и не из `BROOD_GIT_REMOTES`. Обычные `git@host:path` и
`ssh://git@host/path` (без нестандартного порта) нормализуются в HTTPS.
Явный `repository` переопределяет origin; `repository: ""` явно отключает
клонирование. Нет Git/origin и явного значения — обычная сборка отказывает
до обращения к серверу и Docker, не очищая ранее выбранный repository.
Офлайн plan/export при этом разрешены. Userinfo, токены в URL, query,
fragment, локальные пути и IP-адреса не принимаются; секреты в декларацию
не входят.

Обычный `bro build` перед Docker вызывает `PUT /api/projects/sync`.
Администратор создаёт отсутствующий проект или меняет объявленные поля.
Повтор неизменной декларации не записывает строки в БД; его может выполнить
участник проекта без административных прав. Права проверяются сервером
повторно в транзакции. Ошибка авторизации/синхронизации останавливает сборку.
Успешная синхронизация сохраняется, даже если последующая сборка образа
завершилась ошибкой.

Отсутствующие description/capacity сохраняют существующие значения. Явная
capacity полностью заменяет selector и пределы: пропущенные или null пределы
означают unlimited; ноль запрещает новое выделение. Значения — десятичные
строки int64, а не JSON/YAML numbers. Новому проекту без capacity назначается
обычная начальная ёмкость сервера. Членство, секреты и рои не изменяются.
Существующие рабочие копии акторов не очищаются и не переключаются.

Синхронизация repository не выдаёт сетевого доступа. Credentials и разрешённые
направления Git gateway остаются серверной настройкой. Для repository без
доступного режима Git клиент печатает предупреждение; разрешение URL не
превращается в право сервера обращаться к нему. Панель показывает repository
для чтения и указывает Broodfile как источник настройки. Разрешённая
конфигурация проекта сохраняется в `build.json` как `projectConfig`.

## Использование

Как `make`, команда читает `./Broodfile` и текущие файлы из каталога запуска.
Изменённые и новые исходники входят в сборку без commit. `--project NAME`
(или `BUILD_PROJECT`) — имя назначения, не путь к Git checkout. По умолчанию
используется `name` из Broodfile. Флагов `--ref`, `--registry` и `--atespace` нет.
Перед обычной сборкой клиент синхронизирует декларацию проекта с сервером
через текущую `bro login` сессию. Docker собирает образ локально; registry
и публикация по-прежнему не требуются. `--plan` и `--export` работают офлайн:
не читают сессию и не меняют сервер. Обычная сборка в guest-режиме отказывает,
а не использует операторские credentials; plan/export доступны и там.

```sh
bro --help
bro build --help
bro build --plan
bro build coding --export /new/context
bro login https://brood.example.org
bro build
bro build coding
bro push coding --project my-workspace
```

Сборочные опции показаны в `bro build --help`, опции публикации — в
`bro push --help`. Корневая справка перечисляет команды.
Docker/buildx должен поддерживать OCI exporter; для Docker без containerd image
store нужен docker-container builder, выбранный через `BUILDX_BUILDER`.
Клиент не меняет настройки Docker daemon или текущий builder автоматически.

В этом проекте `development` прогоняет тесты и вложенный deploy, `coding`
запускает интерактивный терминал. Обе цели разделяют environment `workbench`.
Внешние Git-источники получаются по закреплённому commit. Клон Substrate
оператора, локальный кластер и login в brood для plan/export не требуются.
Корневой Broodfile использует приватный Substrate-форк: хостовому Git нужен
read access либо явно настроенный проверенный checkout-кэш. Это отдельное
право на исходники, не сессия brood. SSH keys, credential helpers и локальная
Git-конфигурация не попадают в экспортированный source или образ.

## Документ

Корневые поля: `apiVersion`, `name`, `defaultTarget`, `sources`, `environments`,
`templates`, `networks`, `targets`; необязательные `pinsFile` и `workflows`.
Неизвестные поля, YAML aliases/anchors/merge и нестандартные теги запрещены.
Размер документа не более 1 MiB. Вложенные структуры проверяются строгим Go-validator.
Имена — DNS labels. Roles/topologies пока не поддерживаются и не игнорируются.

`workflows` — необязательный словарь именованных графов. Каждый граф содержит
`start`, `exit`, `nodes`, `edges`, `goalGates` и необязательные `styles`, `retry`,
как в standalone-документе `go/brood/pipeline`, но без собственного `name` и
`apiVersion`: имя задаётся ключом словаря, версия — версией Broodfile. Ссылки
`template` в узлах и стилях указывают на `templates` того же Broodfile. Проверка
использует parser и validator конвейера; существующая сборка не запускает граф и
не меняет сборочный план. `bro pipeline validate --workflow inspect [Broodfile]`,
`bro pipeline plan --workflow inspect [Broodfile]` и
`bro pipeline render --workflow inspect [Broodfile] --dot` проверяют, планируют
и отображают выбранный граф; путь по умолчанию — `./Broodfile`. Без флага
`--workflow` команды по-прежнему читают отдельный документ конвейера.
`render` принимает ровно один формат: `--dot` или `--svg`. Оба используют
общий граф, как и панель: подпись ребра — condition, иначе label, с ненулевым
weight. DOT теперь выводит `rankdir=LR`, сортирует рёбра по концам и добавляет
weight в подписи; простые идентификаторы могут быть без кавычек. SVG содержит
только имена узлов и подписи рёбер, не prompt и другие поля узлов.
Безопасный пример — `examples/Broodfile`: offline-шаблон запускает actor с
запросом только на чтение, затем человек принимает или отклоняет результат.
Оба ответа ведут к выходу со статусом прогона `succeeded`; отклонение
сохраняется в ответе как `fail`, но не делает прогон неуспешным. Неуспех actor
завершает прогон со статусом `failed`, не переходя к вопросу. Само объявление
не запускает actor; plan и render выполняются локально без сервера. При реальном
запуске требуются ресурсы actor, а запрет изменения файлов задан в prompt, не
является системной гарантией изоляции файловой системы.

Машиночитаемый контракт для редакторов —
`go/brood/build/broodfile.schema.json` (JSON Schema 2020-12). Он генерируется
из тех же структурных ограничений, что использует Go-validator; тест сравнивает
артефакт с ними, включая нулевые пределы и составные условия. Проверки YAML,
межобъектных ссылок, безопасных путей, Git URLs и диапазона uint64 дополнительно
выполняет компилятор: JSON Schema не заменяет `bro build --plan`.

Положительные примеры: корневой `Broodfile` с целями development/coding и
`go/brood/build/testdata/minimal.yaml` для стороннего проекта без nested Docker.
Примеры отказов до сборки:

| Изменение корректного документа | Причина |
|---|---|
| `apiVersion: brood.dev/v2` | неподдерживаемая версия, `const` |
| `topologies: {}` в корне | неизвестное поле, `additionalProperties` |
| `durableDir: {unsupported: true}` в томе | пустой объект обязателен, `maxProperties` |
| `revision: main` у Git source | требуется полный commit SHA |

Для обновления артефакта после изменения validator из `go/`:

```sh
BROOD_UPDATE_SCHEMA=1 go test ./brood/build -run '^TestBroodfileSchemaArtifact$'
go test ./brood/build
```

```yaml
apiVersion: brood.dev/v1alpha1
name: example
defaultTarget: development
sources:
  project:
    type: project
    destination: project
    gitMetadata: false
environments:
  python:
    sources: [project]
    build:
      dockerfile: project/environment/Dockerfile
      platform: linux/amd64
templates:
  python:
    environment: python
    network: offline
    actor:
      resources:
        limits:
          cpuMillicores: '1000'
          memoryBytes: '1073741824'
      containers:
        - name: main
          command: [python3]
          args: [--version]
networks:
  offline: {}
targets:
  development:
    template: python
```

`sources` — именованные входы; `destination` относительно нового пустого контекста.
Абсолютные пути, `..`, пересекающиеся destinations запрещены. `project` — снимок
текущих файлов проекта; допускается один такой источник. `gitMetadata: true`
добавляет заново созданную shallow metadata HEAD и безопасный origin, но не локальный
config, credential helpers и hooks. Сами рабочие файлы не откатываются к HEAD.
Без Git metadata Git-репозиторий для локального источника не требуется.

Внешний источник имеет `type: git`, `repository` (HTTPS/SSH Git URL без пароля
или HTTP userinfo), `destination` и ровно одно из `revision`/`revisionPin`.
`revision` — полный 40-символьный commit SHA; `revisionPin` — ключ из pinsFile.
Локальные checkout-кэши задаются настройкой установки, а не путями в Broodfile.

`environment.sources` выбирает источники контекста. Dockerfile и его соседний
Dockerfile.dockerignore читаются из этого контекста: путь dockerfile относительно
его корня. `build.arguments` — строковые build args, `build.argsFromPins` — список
ключей pinsFile. Коллизия ключей — отказ. Аргументы не предназначены для секретов;
ключи с TOKEN/PASSWORD/SECRET/PRIVATE_KEY/AUTH отвергаются.

`pinsFile` — JSON-объект строковых pins относительно корня проекта. Он читается
из текущих файлов рядом с Broodfile. В Brood общие Python/Go/Substrate pins хранятся
в `brood/dev/pins.json`, откуда их читают CI, provision и сборщик. Объявления
pins не извлекаются из shell-скриптов регулярными выражениями.

`template.actor` — поддерживаемое подмножество protojson ActorTemplate без metadata
и без container.image. Первая версия поддерживает один контейнер и один собранный
образ на шаблон. Ресурсы, argv, env, workerSelector, capabilities, тома и snapshot
policy задаются явно. Нет privileged/hostPath. Поле image подставляет сборщик.

`network` ссылается на сеть: `tls`/`http` содержат `hosts` и `ports`. Только точные
публичные DNS-имена, без URL, IP, wildcard и локальных суффиксов. Пустая сеть —
запрет всего egress. Это описание политики, а не её автоматическое применение.

## Исходники и контекст установки

Broodfile, Dockerfile, pins и локальные исходники берутся из текущего каталога,
без поиска родительского Git-корня. Внешние `type: git` источники по-прежнему
закреплены полным commit. Чистый parser/validator не вызывает процессы или сеть.
Установленные бинарники и собираемый проект могут находиться в разных деревьях.

Локальный образ получает неопубликованное имя `brood.local/<name>/<environment>`.
Это идентификатор артефакта, а не адрес сетевого registry. Настоящее имя образа
назначает сервер во время `push`; адреса и credentials не входят в Broodfile.

## Результат

`bro build [TARGET]` выбирает defaultTarget либо явную цель. `--plan` выводит
проверенный план без публикации, `--export PATH` готовит новый чистый контекст
без Docker. Экспорт содержит `build-plan.json` для диагностики и настоящий
Dockerfile в выбранном source; его нельзя путать с регистрацией в Substrate.

Docker BuildKit экспортирует OCI-архив, не выполняя push. Временный tag уникален;
digest читается из metadata именно этого buildx build, не из изменяемого тега.
Имена шаблонов содержат сокращённый SHA-256 канонического actor spec с image
и target identity. Изменение образа, argv, env, ресурсов или политики даёт новое
имя и не перезаписывает существующий ActorTemplate.

Выходы `.brood/build/<target>/`: `actor-template.json`, `egress-policy.json`,
`build.json`, `image.oci.tar`. Комплект сначала полностью записывается в `.brood/build/.runs`,
затем стабильная ссылка target заменяется атомарно. Старые успешные комплекты
сохраняются. Чужой файл/каталог/ссылка не заменяется; директории выше output не
могут быть symlinks. Параллельные публикации output одной цели сериализуются.
Отказ не оставляет смесь файлов; уже опубликованные blobs не откатываются.
Output исключён из git и не может попасть в следующий контекст.

`build.json` содержит версию схемы/сборщика, целевую платформу, входные revisions,
использованные pins/build args, digest результата и digest шаблона, без секретов
и локальных путей к кэшам. Локальные источники помечены `workingTree: true`;
при запрошенной Git metadata записывается также `dirty`, а `revision` означает
базовый HEAD, не утверждение, что рабочие файлы совпадают с commit.
Loader и библиотека сборки молчат; вывод делает CLI.

Runtime-поля не входят в аргументы/идентичность сборки окружения. При одинаковых
исходниках два шаблона одного environment используют один образный контекст и
кеш. Если образ содержит Git metadata проекта, новый commit действительно меняет
вход образа, даже когда diff commit касается только Broodfile: это не скрывается
ложной гарантией одинакового digest. Mutable package repositories также не дают
битовой воспроизводимости; полный package lock пока не реализован.

Сборка не создаёт atespace, шаблоны, политики, пулы и акторы. Регистрация и запуск —
отдельные операции. При запуске политика должна быть создана до resume.

## Публикация

`bro push [target]` читает уже собранный комплект, не запускает Docker и не
пересобирает исходники. Явная цель позволяет публиковать после изменения или
удаления Broodfile. Без цели `defaultTarget` читается из текущего Broodfile.
Сервер выбирается из последнего `bro login`; сохраняется его CA trust.

`--project NAME` выбирает доступный личный рой по ID/имени. Также можно указать
серверный проект, если в нём ровно один доступный рой. При неоднозначности
нужен ID роя; членство не даёт доступ к чужим роям. Push не создаёт проект/рой
автоматически и не запускает акторов.

OCI-архив читается без распаковки. SHA-256, размеры и ссылки проверяются;
blobs передаются chunks до 32 MiB, существующие blobs пропускаются. Manifest
публикуется только по digest. Шаблон получает серверные atespace и image URI;
чужое namespace/image назначить нельзя. Повторная публикация идентичного шаблона
идемпотентна, другое содержимое под тем же именем даёт конфликт.

Registry не получает внешний listener или Docker credentials. Все операции
идут через `/api/swarms/<id>/builds/<environment>` с session/CSRF и проверкой прав
на каждом запросе. Upload ID связан с конкретной сессией и истекает через
30 минут; registry Location остаётся только на сервере. После перезапуска сервера
незавершённый blob нужно отправить заново; уже завершённые blobs переиспользуются.

Серверу задаются парой `BROOD_REGISTRY_URL` (внутренний origin) и
`BROOD_REGISTRY_IMAGE_HOST` (канонический host для воркеров, у kind `localhost:5002`).
Штатный deploy обнаруживает адрес проверенного registry container в kind network
и разрешает Go BFF egress только на его IP:5000. Без настроек push отвечает 503.
В pinned Substrate `EgressPolicy` вложена в actor, а golden policy — отдельное
preview-поле для golden actor. Template-only push не подменяет ими друг друга:
golden snapshots остаются DISABLED (explicit ENABLED даёт отказ), runtime rules
остаются в локальном `egress-policy.json`. Ответ `egress_policy_deferred: true`
и сообщение CLI явно требуют применить эти правила при будущем создании actor.

## Границы

Локальное копирование исключает `.env`, `.env.*`, `secrets`, `.ate-dev-env.sh`,
`.brood`, `.git`, `venv`, `node_modules`, `inventory.yaml`, `db.sqlite3` и
`brood.db` с WAL/SHM на любой глубине. При `gitMetadata: true`
наличие секретных/генерируемых путей в HEAD вызывает отказ, чтобы не вернуть их
через Git objects. Локальный config и credential-bearing origin не переносятся.
Ссылки не могут выходить за корень или указывать на исключённые пути; специальные
файлы отвергаются. Это не сканер содержимого: исходники и Dockerfile должны быть
доверенными и не содержать credentials.

Окружение Brood описано корневым Broodfile: development выполняет существующий
`.ci/workbench.sh`, coding запускает CLI/ttyd/tmux. Подготовка cgroup, kmsg и
приватной ext4 остаётся в `brood/dev/workbench/runtime.py`/`storage.py`.
Обычный проект не наследует эти права. Suspend/resume не считается проверенным
результатом build. Детали запуска workbench — в `docs/WORKBENCH.md`.
