# SUBSTRATE

Локальный Agent Substrate для разработки: отдельный кластер kind, собранный
из checkout'а substrate. Команды — `brood substrate`, код —
`brood/dev/substrate` (порт `hack/substrate.sh` из checkout'а substrate).

```sh
brood substrate install    # собрать из checkout'а и раскатать; повтор раскатывает изменения
brood substrate stop       # остановить, данные кластера остаются
brood substrate start      # поднять и дождаться готовности
brood substrate uninstall  # удалить кластер вместе с данными
```

## Настройка

`SUBSTRATE_SOURCE` в `.env` — путь к клону agent-substrate/substrate. Дефолта
нет: путь — свойство машины. Нужны Linux, Docker и Go (для `install`).

Ядро Docker должно уметь IPv6, хотя кластер IPv4: Envoy слушает dual-stack.
Хост, загруженный с `ipv6.disable=1`, этот dataplane не поднимет. Команда
проверяет это до раскатки и настроек хоста не меняет.

## Что создаётся

- кластер kind `substrate` (контейнер `substrate-control-plane`), конфиг —
  `brood/dev/substrate/kind.yaml`;
- реестр `substrate-registry` на `127.0.0.1:5002`, в него `ko` кладёт образы;
- Substrate в `ate-system`: gVisor, dataplane envoy. Ingress пускает
  identity ServiceAccount `ate-system/ate-client`; credential provider
  стартует со своей политикой default-deny по namespace;
- `<checkout>/bin/kubectl-ate`.

Состояние — в `<checkout>/bin/substrate`: kubectl нужной версии, kubeconfig и
записи владения (`owned`, Id контейнеров). Там же его держал
`hack/substrate.sh`, поэтому установка скрипта и установка brood — одна и та
же. Текущий контекст kubectl не меняется:

```sh
KUBECONFIG=<checkout>/bin/substrate/kubeconfig <checkout>/bin/substrate/kubectl get pods -A
KUBECONFIG=<checkout>/bin/substrate/kubeconfig <checkout>/bin/kubectl-ate get actors
```

## Владение

Имена контейнеров постоянные. Каждая команда до первой правки сверяет метки
контейнеров и их Id с записями владения и отказывает, если контейнер чужой
или подменён. `install` не заменяет контейнеры с этими именами, которых он
не создавал.

`stop` сохраняет хранилище кластера, но не снимает снимок памяти идущих
акторов. `uninstall` оставляет на диске скачанные инструменты и кэши сборки.
