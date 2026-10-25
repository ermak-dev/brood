# LXC

Экспериментальный класс `lxc` в форке Agent Substrate для Brood.
Актор работает в настоящем LXC-контейнере с общим ядром хоста.
Класс `fullvm` отменён. Submodule в brood не добавляем.

Код рантайма и его задачи: https://git.ermak.dev/ermak/substrate.
GitHub upstream: https://github.com/agent-substrate/substrate.
Код интеграции и его задачи остаются в brood.

## Архитектура

Новый `cmd/ateworker-lxc` работает подом обычного WorkerPool и реализует
`internal/proto/ateworkerpb` (`service Worker`). Штатный atelet готовит
образы, ActorDirs и тома, затем вызывает воркер по unix-сокету из
`internal/nodepath`. Диалер ate-api-server и pod identity сохраняются.

LXC запускает rootfs, подготовленный atelet. В первой версии воркер
объявляет ёмкость в один актор. Это ограничение MVP, не требование протокола.
Окружение с systemd и вложенным Docker нужно проверить отдельно от запуска
обычного процесса из OCI-образа.

Сеть планируем построить на `internal/ateworker/actornet` и штатном atunnel:
отдельный netns, veth и политики egress. Совместимость с LXC и запрет обхода
туннеля пока не проверены. Общая библиотека не даёт этих гарантий сама по себе.

## Что известно из кода

- `cmd/atelet/main.go`: воркер доступен по `WorkerSocketPath(podUID)`.
  Новый рантайм не требует переписывать atelet, но его предположения об
  OCI rootfs, mounts и sandbox assets нужно проверить на LXC.
- `cmd/atecontroller/internal/controllers/workerpool_apply.go`:
  `workerSecurityContext` и `maybeApplyMicroVMPodShape` задают форму пода по
  классу. LXC потребует своей настройки. Для спайка допустим privileged-под,
  но права производственного воркера выбираем после проверки.
- `cmd/atecontroller/internal/workersync/syncer.go`: workersync создаёт
  Worker из Pod и на старте обходит реестр. Внешние записи не имеют отдельного
  признака владения. Не рассчитываем сохранить запись демона без Pod.
- `cmd/ateapi/internal/controlapi/dialer.go`: atelet выбирается из информера
  Pod по ноде, серверный сертификат проверяется по pod UID.
- `cmd/ateapi/internal/controlapi/workflow_suspend.go` переводит актора в
  SUSPENDING до вызова Checkpoint. `handleAteletError` в `crash.go` считает
  Unimplemented причиной CRASHED. Просто вернуть его из воркера недостаточно.

Новый класс затронет `pkg/sandboxclass`, CRD enum и генерацию, валидацию API,
SandboxConfig и runtime assets, форму пода и сборку образа воркера, реестр
метрик и тесты. Полный список определяем по вызывающему коду, а не по числу
совпадений enum.

## Деградация гарантий

| Область | Условия для lxc |
|---|---|
| Изоляция | Общее ядро, слабее gVisor и microVM. Только доверенные нагрузки |
| Suspend и resume | В MVP нет сохранения памяти и восстановления из checkpoint |
| Снапшоты и теги | Операции, требующие checkpoint, отклоняются без изменения живого актора |
| Отказ ноды | Нет гарантии восстановления выполнявшегося процесса. Состояние API и уборку проверяем тестами |
| Данные | Обычный writable rootfs не считаем долговечным. Переживание сбоя зависит от выбранного тома |
| Сеть и identity | Планируем штатные atunnel и политики. Подтверждаем e2e до заявления о гарантиях |

Число доверенных тенантов не является мерой безопасности. Ошибка в LXC или
привилегированном воркере может затронуть ноду и другие нагрузки на ней.
Класс не поддерживает враждебную мультиарендность upstream.

## Этапы

### L0: проверка запуска

На отдельной тестовой ноде или в VM поднять вложенный LXC из пода.
Проверить cgroup v2, AppArmor, UID mappings, mounts, запуск и остановку
процесса, затем systemd и вложенный Docker. Проверить доступ к HTTP-порту
через atunnel и запрет прямого выхода в сеть и соседние нагрузки.

Записать версии ядра, LXC, container runtime, настройки пода и результаты.
Не менять основной PVE-хост. Go/no-go принимаем по измеренному результату,
а не по оценке срока.

### L1: MVP

1. Сначала обеспечить безопасный отказ suspend и других checkpoint-операций
   для `lxc`: живой актор и его назначение сохраняются.
2. Добавить класс, SandboxConfig/runtime assets и сборку образа воркера.
3. Реализовать RunWorkload, TerminateWorkload, статистику, регистрацию epoch,
   crash reporting и остановку воркера. Переиспользовать register, atunnel,
   actorlog, workerstats и cgroup там, где подходят их контракты.
4. Проверить жизненный цикл через ate API: create, ingress, egress, delete,
   restart, crash и отказ неподдерживаемых операций. Остальные классы не меняются.

CheckpointWorkload и RestoreWorkload не имитируют сохранение памяти.
Отказ на уровне API должен сработать до вызова этих RPC.

### L2: размещение на Proxmox

Сначала проверить схему в отдельном PVE-стенде. Вариант с kubelet на PVE-хосте
затрагивает CNI, firewall, vmbr и cgroups. Это изменение хоста требует
отдельного согласования и плана отката. Безопаснее начать с выделенной VM.

### L3: необязательная интеграция PVE CT

LXC из `lxc-start` не становится управляемым Proxmox CT автоматически.
Создание через `pct`, PVE storage и видимость в UI требуют отдельного этапа.
Совместимость netns, rootfs и mounts с контрактом atelet пока не доказана.

Дисковый backup или snapshot PVE не равен checkpoint памяти.
Не подменяем им SuspendActor. Доступность snapshots зависит от backend,
версии PVE и конфигурации томов, это проверяется на стенде.

## Разработка и интеграция с brood

Substrate остаётся отдельным Go-репозиторием. `origin` указывает в GitLab,
`upstream` в GitHub. Историю upstream сохраняем, module path не меняем.
Задачи рантайма ведём в трекере Substrate по процессу его CLAUDE.md.

Brood уже поддерживает `SUBSTRATE_SOURCE` для рабочего клона и закрепляет
`REPOSITORY`/`COMMIT` в `brood/dev/substrate/local.py` для установки.
Переход на форк делаем отдельной задачей после проверки совместимости.
Состояние локального кластера остаётся вне эфемерного checkout CI,
см. [SUBSTRATE.md](SUBSTRATE.md).

`brood/server/projects/pools.py` пока разрешает только `gvisor`.
Поддержку `lxc` добавляем вместе с проверками API, панелью и объяснением
ограничений класса. Автоматический suspend для таких акторов не включаем.

GitLab CI форка настраиваем отдельно: Python-проверки brood не проверяют Go.
Перенос документа и создание форка ещё не означают, что LXC работает.

## Задачи

[Эпик Substrate #1](https://git.ermak.dev/ermak/substrate/-/issues/1):

- #3: L0, проверка вложенного LXC.
- #4: безопасный отказ checkpoint-операций.
- #5: класс и конфигурация пода.
- #6: lifecycle и статистика воркера.
- #7: end-to-end проверки.
- #8: GitLab CI форка.
- #9 и #10: отложенные PVE-стенд и интеграция pct.

Номера выше относятся к проекту Substrate.
[Brood #10](https://git.ermak.dev/ermak/brood/-/issues/10) ведёт подключение
форка к установке и панели пулов.
