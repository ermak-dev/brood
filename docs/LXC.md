# LXC

Экспериментальный `lxc` — настоящий LXC-контейнер с общим ядром ноды.
Только доверенные нагрузки: изоляция слабее gVisor и microVM, для враждебной
мультиарендности класс не предназначен. `fullvm` отменён, submodule не нужен.

## Исходники и pins

Runtime живёт в форке https://git.ermak.dev/ermak/substrate; upstream —
https://github.com/agent-substrate/substrate. Installer и корневой Broodfile
закрепляют проверенный commit `0d435f2e2ff86b73fc9982d9aac3fa77d7b2ed9f` ключом
`SUBSTRATE_COMMIT` в `brood/dev/pins.json`.

Публичный Go SDK сервера/клиента остаётся на `4ebd668d62e3f55753765a46579fc6deb81b2f7f`
(`SUBSTRATE_SDK_COMMIT` и `go.mod`). Он предоставляет существующий protobuf/gRPC
контракт; приватный fork не становится Go module dependency. Замены на
машинный checkout нет. Runtime pin и SDK pin проверяются отдельно.

Форк приватный. Git читает его на хосте по credential-free SSH URL; нужны
read access или проверенный source cache. Ключи, credential helpers и локальный
Git config не входят в source export и не переносятся в актор. Installer
сохраняет `SUBSTRATE_SOURCE` как явный developer override без fetch/checkout.
Владение и состояние кластера остаются в постоянном каталоге, не в релизе CI
(`docs/SUBSTRATE.md`). Старый managed clone с другим origin не переписывается
автоматически и не удаляется вместе с записями владения.

## Совместимость существующей установки

В `f5d03ca0` PostgreSQL-миграции до v0.6.0 сведены в новый
`000001_initial.sql`. Старую pre-squash базу обновлять нельзя: это явно
зафиксировано в `docs/dev/postgresql-schema-evolution.md` форка. Совпадение
protobuf/gRPC-контракта не означает совместимость истории базы.

Не удаляйте рабочую базу и не редактируйте `schema_migrations` ради запуска
нового control plane. Source override не является миграцией данных. Для
проверки нового runtime нужна новая изолированная установка. Один дополнительный
namespace на прежней ноде не изолирует cluster-wide controllers, CRD и atelet:
второй atelet использует тот же `/var/lib/ate` и worker-support socket.

Workbench позволяет проверять новую установку внутри собственного Docker и
kind в отдельной microVM актора, без сокета, kubeconfig и credentials рабочей
установки. Это не обновление production Substrate и не разрешение создавать
дополнительные VM/ноды на хосте, сбрасывать пользовательские акторы или менять
host kernel, CNI и firewall. Положительный fresh-install proof необходимо
отличать от ещё не реализованного обновления pre-squash базы с сохранением данных.

## Что реализовано в форке

`cmd/ateworker-lxc` — обычный WorkerPool pod с Worker RPC и штатной регистрацией,
epoch, статистикой, crash reporting и остановкой. Atelet готовит OCI rootfs и
тома; LXC worker строит OCI config из разрешённого ContainerSpec. Сеть использует
`actornet`, отдельный netns, штатный atunnel и инфраструктурную identity.

Образ воркера строится `cmd/ateworker-lxc/build.sh` из его Dockerfile: нужны
LXC userspace, iproute2, nftables, util-linux, uidmap и сертификаты. Обычный
ko/distroless образ для этого класса непригоден. SandboxConfig `lxc-image`
содержит версию `image`, пустые runtime assets для amd64/arm64: нужные runtime
бинарники уже находятся в образе воркера.

Один воркер объявляет ёмкость в один актор. Это ограничение MVP, не протокола.
Cold start/resume после отсутствия процесса не равен восстановлению RAM.
Операции, требующие memory checkpoint, отклоняются в API до изменения живого
актора, его назначения и томов. Автоматический suspend brood не включает.

## Доверенный workbench-профиль

Метка `ate.dev/lxc-nesting=enabled` в `WorkerPool.metadata.labels` явно включает
профиль вложенных Docker/kind. Он добавляет FSETID, KILL, AUDIT_WRITE и SYSLOG,
а также запрос одного ресурса `ate.dev/kmsg`. Штатный device plugin atelet
передаёт только настоящий `/dev/kmsg` с правом чтения и соответствующим
device-cgroup allow. Одной capability или hostPath для этого недостаточно.
Worker pod остаётся непривилегированным; обычные LXC-пулы и остальные классы
не расширяются. Профиль допускает чтение общего kernel log доверенной нагрузкой.

Обычный LXC-актор сохраняет лимит 512 задач cgroup. Nesting-профиль передаёт
воркеру `--actor-pids-limit=4096`; это лимит актора, не изменение PID policy ноды.
В обоих корневых профилях `GOMAXPROCS=4` и `GOFLAGS=-p=2` ограничивают
параллелизм компиляции. Atelet имеет только DAC_OVERRIDE и FOWNER для удаления
actor-owned файлов, в том числе из sticky-каталогов; его контейнер остаётся
непривилегированным, escalation отключён.

Перед установкой нового контроллера соберите совместимый LXC worker image:
старый образ не понимает `--actor-pids-limit`. При отсутствии opt-in пулов
можно подготовить образ, установить runtime, явно обновить LXC digest в
`WORKER_POOL_IMAGES` и только затем создать отдельный nesting-пул. Существующие
обычные пулы не переводятся на новый образ автоматически.

Переключение метки вызывает rollout воркеров и может вытеснить акторы.
Используйте отдельный пустой пул, не меняйте занятый пользовательский пул.
Недоступный на ноде kernel-log device означает отсутствие требуемого ресурса,
а не разрешение расширить права всей VM. Подготовка actor-local devpts и
ext4 workspace описана в `docs/WORKBENCH.md`; полный live-прогон отслеживает #96.

## Гарантии и ограничения

| Область | Действующий контракт |
|---|---|
| Изоляция | Общее ядро; только доверенные нагрузки |
| Suspend/checkpoint | Нет сохранения RAM; отказ не должен повреждать живого актора |
| Отказ ноды | Автоматическое восстановление выполнявшегося процесса не гарантируется |
| Rootfs | Временный writable rootfs не считается долговечным |
| Данные | Переживание сбоя зависит от выбранного volume; CSI проверялся отдельно |
| Сеть | Штатные ingress/egress gateways и отдельные actor netns; запрет прямого обхода проверяется e2e |
| Права воркера | Настройки ограничены LXC-классом; минимальность набора прав не доказана |

Наличие нескольких доверенных тенантов не делает общую ноду границей
безопасности. Ошибка LXC/воркера может затронуть другие нагрузки ноды.
Настройки host firewall, CNI, PVE, vmbr и sysctls не меняются ради интеграции.

## Что проверено

В Substrate завершены L0, безопасный отказ checkpoint, класс/CRD, lifecycle,
API e2e и GitLab CI (эпик #1, задачи #3–#8). На выделенном стенде проверены
systemd и вложенный Docker, настоящий ingress через mTLS CONNECT, разрешённый
и запрещённый egress, отдельные netns, CSI/rootfs/RAM-маркеры, sync/async suspend
refusal, cold reset, crash, пересоздание worker Pod и cleanup без повреждения
соседа. API suite прошёл `-race -count=3 -shuffle=on`; main pipeline6307 SUCCESS.

Воспроизведение, версии стенда и границы — `hack/lxc-e2e/README.md` форка.
Live gVisor/microVM e2e на том стенде не запускались; их fixture-регрессии
прошли. Чистые проверки Go-модуля brood не заменяют эти runtime-тесты.

Brood #10 отдельно требует настоящее создание LXC-пула и актора через brood
и проверку gVisor. Наличие UI option, fake Kubernetes в браузерном тесте или
одна сборка образа сами по себе этот критерий не выполняют.

## Интеграция Brood

- Go API принимает `lxc` только из явно установленной digest-pinned карты
  `WORKER_POOL_IMAGES`; `gvisor` остаётся обязательным и default. Произвольные
  pod-поля, образы, ServiceAccount и аннотации по-прежнему отвергаются.
- Новый LXC pool без `configRef` получает явный `lxc-image`; custom config
  сохраняется. Update существующего pool не добавляет ссылку и не меняет класс.
- Подготовка (`brood/dev/deploy/lxc.py`) сначала проверяет Id/метки контейнеров,
  namespace и поддержку класса в установленном CRD. Затем собирает образ,
  повторно проверяет владение перед push и создаёт только свой SandboxConfig.
  Перед native write владение проверяется снова. Временный tag очищается только
  при совпадении Image ID; наружу возвращается registry digest.
- Глобальный class-default не назначается: newest-default мог бы перепривязать
  существующие LXC-пулы. Чужой, terminating или изменённый `lxc-image` не
  перезаписывается. Другие configs и пулы не меняются.
- `brood deploy lxc --trusted-workloads` готовит образ/config, но не создаёт
  WorkerPool и не останавливает gVisor/microVM. Без явного trusted-only
  подтверждения команда отказывает до lookup кластера. `--help` перечисляет
  shared-kernel/no-checkpoint ограничения без побочных эффектов. Полученный
  digest добавляют в `WORKER_POOL_IMAGES` явно; это не автоматический rollout.
- Server deploy не устанавливает новый runtime и не переключает пулы. Сборка
  LXC-образа и установка fork control plane — явные действия, не побочный эффект
  выкладки Go-сервера.
- Панель показывает только установленные классы, сохраняет runtime/image/config
  существующего пула и объясняет shared-kernel/no-checkpoint ограничения во всех
  шести локалях. Обычным пользователям инфраструктурные меню не открываются.

Runtime-приёмка Brood #10 завершена 2026-10-30 в отдельной workbench-microVM
с собственным Docker/kind и свежей установкой `f5d03ca0`. Приватный Go Deployment
создал через настоящий Brood API отдельные LXC- и gVisor-пулы, шаблоны и акторы.
Оба актора вернули ожидаемый ответ через mTLS actor-view; фактические runtime
classes проверены. LXC suspend вернул 409 до изменения актора: UID, состояние
и boot ID LXC, а также boot ID соседнего gVisor сохранились. Повторная подготовка
LXC сохранила полное содержимое SandboxConfig, UID, resourceVersion и generation.

Этот отдельный runtime-прогон использовал исходный workbench `4f57f2c` с явно
наложенными исправлениями #83, #84 и #86; его нельзя выдавать за чистый корневой
прогон. Go source tree `a4f1cace8f837fb8cd8a66316d92492c1f4f85ff` совпадает
с итоговым `026963f1`. Проверенный приватный образ сервера —
`localhost:5002/brood-go@sha256:1a8a948db653efac791fc55dcac5744c6354f3439297706cf96e7859b6afae00`.
HTTPS проверен с CA и именем. После успеха удалены дочерние акторы, шаблоны,
рой, проект и пулы, затем выполнен uninstall вложенного кластера; только после
этого удалена внешняя проверочная среда. Чистая корневая development/coding
приёмка итогового commit описана отдельно в `docs/WORKBENCH.md`.

В приёмке 2026-10-30 production runtime оставался на прежнем публичном pin:
тот результат доказывал fresh install, но не обновление старой PostgreSQL.
База и migration ledger не
сбрасывались. Пользовательские coding-акторы не использовались, исходный пул
не переключался, host runtime policy и PVE не менялись. Нехватка ёмкости
по-прежнему не разрешает останавливать пользовательские акторы, уменьшать
их ресурсы или создавать VM/ноду без отдельного согласования.

2026-10-31 пользователь отдельно разрешил удалить старые данные и создать
установку заново (#95). Удалены только проверенные kind-кластер и registry
с их томами; `.env`, TLS и доступ выкладки сохранены. На свежем `f5d03ca0`
создан настоящий LXC-актор через Brood: проверены RUNNING, runtime class lxc
и HTTP 200 через защищённый actor-view. Это не data-preserving migration
старой базы и не доказательство полного LXC development/coding workbench.

## Proxmox не входит в MVP

L2 (отдельный PVE-стенд) и L3 (`pct`, PVE storage, видимость CT в UI) остаются
parked в форке, задачи #9/#10. Контейнер от `lxc-start` не становится
управляемым PVE CT. Дисковый backup/snapshot не является memory checkpoint.
Изменение PVE-хоста требует отдельного согласования и плана отката; этот код
не выполняет таких изменений.
