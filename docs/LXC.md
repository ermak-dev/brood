# LXC

Экспериментальный `lxc` — настоящий LXC-контейнер с общим ядром ноды.
Только доверенные нагрузки: изоляция слабее gVisor и microVM, для враждебной
мультиарендности класс не предназначен. `fullvm` отменён, submodule не нужен.

## Исходники и pins

Runtime живёт в форке https://git.ermak.dev/ermak/substrate; upstream —
https://github.com/agent-substrate/substrate. Installer и корневой Broodfile
закрепляют проверенный commit `f5d03ca0e49be0c429d892437ab5b93924026af0` ключом
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

Полный runtime proof brood остаётся отдельным критерием #10; production
пользовательские coding-акторы не используются для проверки. Нехватка ёмкости
не разрешает останавливать их, уменьшать ресурсы или создавать VM/ноду без
отдельного согласования.

## Proxmox не входит в MVP

L2 (отдельный PVE-стенд) и L3 (`pct`, PVE storage, видимость CT в UI) остаются
parked в форке, задачи #9/#10. Контейнер от `lxc-start` не становится
управляемым PVE CT. Дисковый backup/snapshot не является memory checkpoint.
Изменение PVE-хоста требует отдельного согласования и плана отката; этот код
не выполняет таких изменений.
