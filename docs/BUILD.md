# Build

Broodfile описывает сборочные входы и шаблоны проекта. Версия первой схемы —
`brood.dev/v1alpha1`; схема лежит в `brood/dev/build/schema.json`. Backend первой
версии — Dockerfile/BuildKit. Bootstrap внутри актора не конфигурирует runtime
Substrate и не выполняется при сборке.

## Использование

В `.env` установки задайте `BUILD_REGISTRY` и `BUILD_ATESPACE`; они также
принимаются флагами `--registry` и `--atespace`. Для принадлежащего установке
локального реестра адрес — `localhost:5002`. Broodfile должен находиться в
выбранном commit, а не только в рабочем дереве.

```sh
brood build --plan
brood build
brood build coding
brood build coding --project /path/to/project --ref HEAD --export /new/context
```

В этом проекте `development` прогоняет тесты и вложенный deploy, `coding`
запускает интерактивный терминал. Обе цели разделяют environment `workbench`.
Для собственной рабочей копии Substrate задаётся `BUILD_SOURCE_CACHE`: JSON
map URL репозитория в абсолютный путь локального клона. Без этой настройки
сборщик получает закреплённый commit по сети.

## Документ

Корневые поля: `apiVersion`, `name`, `defaultTarget`, `sources`, `environments`,
`templates`, `networks`, `targets`; необязательное `pinsFile`.
Неизвестные поля, YAML aliases/anchors/merge и нестандартные теги запрещены.
Размер документа не более 1 MiB. Вложенные структуры проверяются схемой.
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
выбранного commit проекта; допускается один такой источник. `gitMetadata: true`
сохраняет настоящий shallow commit и безопасный origin, но не локальный config,
credential helpers и hooks. Без Git metadata в образ идут только файлы снимка.

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
из того же commit, что Broodfile. В Brood общие Python/Go/Substrate pins хранятся
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

Команда ищет Git-корень от текущего каталога или `--project PATH` и читает Broodfile,
Dockerfile, pins и исходники из `--ref` (по умолчанию HEAD). Незакоммиченные tracked
и untracked изменения не накладываются на снимок; команда сообщает их исключение.
Сам установленный Brood и целевой проект могут быть разными деревьями.

Registry передаётся `--registry` либо настройкой установки `BUILD_REGISTRY`;
atespace — `--atespace` либо `BUILD_ATESPACE`. Отсутствие — отказ до сборки.
Адреса/credentials установки не зашиваются в декларацию. Первая реализация
публикует только в registry проверенной LocalCluster; проверка владения предшествует
публикации. Read-only Git/файловые операции при загрузке не являются выполнением
сборки: чистый parser/validator не вызывает процессы или сеть.

Опциональный `BUILD_SOURCE_CACHE` — JSON map безопасного repository URL в путь
локального checkout установки. Кэш только ускоряет получение проверенного commit;
его рабочие изменения не копируются и origin должен соответствовать repository.
Без кэша внешний commit запрашивается у указанного repository.

## Результат

`brood build [TARGET]` выбирает defaultTarget либо явную цель. `--plan` выводит
проверенный план без публикации, `--export PATH` готовит новый чистый контекст
без Docker. Экспорт содержит `build-plan.json` для диагностики и настоящий
Dockerfile в выбранном source; его нельзя путать с регистрацией в Substrate.

Имя repository: `<registry>/<project>/<environment>`. Временный tag уникален для
вызова; digest читается из metadata именно этого buildx build, не из изменяемого
тега. Имена шаблонов содержат сокращённый SHA-256 канонического actor spec с image
и target identity. Изменение образа, argv, env, ресурсов или политики даёт новое
имя и не перезаписывает существующий ActorTemplate. Atespace берётся из контекста.

Выходы `.brood/build/<target>/`: `actor-template.json`, `egress-policy.json`,
`build.json`. Комплект сначала полностью записывается в `.brood/build/.runs`,
затем стабильная ссылка target заменяется атомарно. Старые успешные комплекты
сохраняются. Чужой файл/каталог/ссылка не заменяется; директории выше output не
могут быть symlinks. Параллельные публикации output одной цели сериализуются.
Отказ не оставляет смесь файлов; уже опубликованные blobs не откатываются.
Output исключён из git и не может попасть в следующий контекст.

`build.json` содержит версию схемы/сборщика, целевую платформу, входные revisions,
использованные pins/build args, digest результата и digest шаблона, без секретов
и локальных путей к кэшам. Loader и библиотека сборки молчат; вывод делает CLI.

Runtime-поля не входят в аргументы/идентичность сборки окружения. При одинаковых
исходниках два шаблона одного environment используют один образный контекст и
кеш. Если образ содержит Git metadata проекта, новый commit действительно меняет
вход образа, даже когда diff commit касается только Broodfile: это не скрывается
ложной гарантией одинакового digest. Mutable package repositories также не дают
битовой воспроизводимости; полный package lock пока не реализован.

Сборка не создаёт atespace, шаблоны, политики, пулы и акторы. Регистрация и запуск —
отдельные операции. При запуске политика должна быть создана до resume.

## Границы

Корневые пути настроек установки `.env`, `secrets/`, `.ate-dev-env.sh`, `.brood/`
в commit — отказ, не молчаливый пропуск. Игнорируемые Git настройки не копируются;
credential-bearing origins и локальный Git config тоже не переносятся. Вложенные
файлы библиотек, например публичная `vendor/.../.env` с тестовыми данными, не
считаются настройками установки по одному имени. Это не сканер содержимого на
секреты: исходники и Dockerfile должны быть доверенными и не содержать credentials.
Ссылки в снимке не могут указывать за его границы.

Окружение Brood описано корневым Broodfile: development выполняет существующий
`.ci/workbench.sh`, coding запускает CLI/ttyd/tmux. Подготовка cgroup, kmsg и
приватной ext4 остаётся в `brood/dev/workbench/runtime.py`/`storage.py`.
Обычный проект не наследует эти права. Suspend/resume не считается проверенным
результатом build. Детали запуска workbench — в `docs/WORKBENCH.md`.
