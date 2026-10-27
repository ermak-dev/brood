# SUBSTRATE

Локальный Agent Substrate: отдельный кластер kind, собранный из checkout'а
substrate. Команды — `brood substrate`, код — `brood/dev/substrate` (порт
`hack/substrate.sh`).

```sh
brood substrate install    # собрать из checkout'а и раскатать; повтор раскатывает изменения
brood substrate stop       # остановить, данные кластера остаются
brood substrate start      # поднять и дождаться готовности
brood substrate uninstall  # удалить кластер вместе с данными
```

## Откуда Substrate

Без настроек `install` ставит официальный репозиторий
github.com/agent-substrate/substrate на коммите `COMMIT` из
`brood/dev/substrate/local.py`. Свой клон лежит в каталоге данных
пользователя, `$XDG_DATA_HOME/brood/substrate` (по умолчанию
`~/.local/share/brood/substrate`), а не в рабочей копии brood: записи владения
кластера в `<checkout>/bin/substrate` переживают джобу CI. `install` заводит
клон или переводит его на `COMMIT`; клон с правками в отслеживаемых файлах или
с другим origin — отказ. `start`, `stop` и `uninstall` клон не трогают и в сеть
не ходят.

Новый Substrate — правка `COMMIT`, после того как флаги `DEPLOY` сверены с
`hack/install-ate-kind.sh` этого коммита. `DEPLOY` также держит
`--ateapi-authz-bootstrap-owners`: глобальные владельцы ateapi, устанавливает
их `ate-setup` флагом на Deployment ate-api-server. Go-сервер использует
историческую ServiceAccount `django` (`system:serviceaccount:brood:django`):
CRUD atespace требует global owner. Это infrastructure identity, не Django
runtime. Brood проверяет пользовательские права перед вызовом привилегированного
ateapi. Общий `ate-client` оставлен: без него `kubectl ate` и демо не работают.

`SUBSTRATE_SOURCE` в `.env` — свой клон разработчика вместо этого: он берётся
как есть, без fetch и checkout.

## Настройка

Нужны Linux, Docker, git и Go (для `install`; нужный Substrate toolchain Go
скачивает сам).

Ядро Docker должно уметь IPv6, хотя кластер IPv4: Envoy слушает dual-stack.
Хост, загруженный с `ipv6.disable=1`, этот dataplane не поднимет. Команда
проверяет это до раскатки и настроек хоста не меняет.

## Порты хоста

`BROOD_HTTPS_PORT` и `BROOD_HTTP_PORT` в `.env` -- порты хоста, на которые
узел публикует вход сервера brood (NodePort `30443` и `30080`,
`docs/DEPLOY.md`). Не заданы -- ничего не публикуется, сервер достают
`kubectl port-forward`. kind задаёт порты только при создании кластера:
`install` над узлом с другими портами отказывает и называет пересоздание
(`uninstall`, затем `install`), сам кластер не стирает.

## Что создаётся

- кластер kind `substrate` (контейнер `substrate-control-plane`), конфиг —
  `brood/dev/substrate/kind.yaml`;
- реестр `substrate-registry` на `127.0.0.1:5002`, в него `ko` кладёт образы;
  containerd и atelet обращаются к нему как `substrate-registry:5000`. После
  upstream deploy установка согласует флаг `--localhost-registry-replacement`
  atelet с этим именем, не меняя остальные аргументы и чужие контейнеры;
- Substrate в `ate-system`: gVisor, dataplane envoy. Ingress пускает
  identity ServiceAccount `ate-system/ate-client`; credential provider
  стартует со своей политикой default-deny по namespace;
- `<checkout>/bin/kubectl-ate`.

`brood substrate install` ставит инфраструктуру без демо и без пулов.
Подготовка Go endpoint не создаёт и не меняет существующие WorkerPool
(см. `docs/DEPLOY.md`). Пулы создаются осознанно через авторизованный API
или инструменты Substrate; в минимальной Go-панели редактора пулов нет.
WorkerPool задаёт число подов и их ресурсы; Worker в ateapi — отражение
конкретного пода, а ActorTemplate задаёт нагрузку внутри sandbox, не заменяет пул.

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
