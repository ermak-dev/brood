# Build

Broodfile описывает сборочные входы и шаблоны проекта. Версия первой схемы —
`brood.dev/v1alpha1`; строгий parser/validator — `go/brood/build/plan.go`. Backend первой
версии — Dockerfile/BuildKit. Bootstrap внутри актора не конфигурирует runtime
Substrate и не выполняется при сборке.

## Использование

Как `make`, команда читает `./Broodfile` и текущие файлы из каталога запуска.
Изменённые и новые исходники входят в сборку без commit. `--project NAME`
(или `BUILD_PROJECT`) — имя назначения, не путь к Git checkout. По умолчанию
используется `name` из Broodfile. Флагов `--ref`, `--registry` и `--atespace` нет.
Login, кластер и registry для локальной сборки не нужны.

```sh
bro --help
bro build --help
bro build --plan
bro build
bro build coding
bro build coding --export /new/context
bro login https://brood.example.org
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
оператора, локальный кластер и login для plan/export не требуются.

## Документ

Корневые поля: `apiVersion`, `name`, `defaultTarget`, `sources`, `environments`,
`templates`, `networks`, `targets`; необязательное `pinsFile`.
Неизвестные поля, YAML aliases/anchors/merge и нестандартные теги запрещены.
Размер документа не более 1 MiB. Вложенные структуры проверяются строгим Go-validator.
Имена — DNS labels. Roles/topologies пока не поддерживаются и не игнорируются.

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
