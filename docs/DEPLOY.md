# DEPLOY

Зелёный `main` раскатывается сам: джоба `deploy` пайплайна ставит на машину
деплоя Substrate тем же `brood substrate install`, что у разработчика
(`docs/SUBSTRATE.md`), затем `brood substrate start` и сервер brood --
`brood deploy server`. Админка стенда -- `https://brood.corp.ermak.dev/admin/`
и `https://brood/admin/`.

## Как идёт выкладка

Джоба идёт на раннере `builder` (shell, тег `docker`), как выкладка mop:

1. `.ci/deploy.sh` шлёт по ssh `git archive` коммита и его sha пользователю
   `brood` машины деплоя; к дереву дописаны секреты стенда: `.env` и
   `secrets/tls.crt`, `secrets/tls.key`, с правами 0600;
2. в `authorized_keys` у этого ключа
   `restrict,from="<builder>",command="/usr/local/bin/brood-deploy"`: клиент
   никакой команды не выбирает, ключ принимается только с builder и умеет
   только выкладку;
3. `brood-deploy` (`deploy/brood-deploy.sh`) распаковывает дерево в
   `~brood/releases/<sha>` и зовёт из него `bin/brood substrate install`,
   `bin/brood substrate start` и `bin/brood deploy server`. Вывод идёт в лог
   джобы, код выхода -- её код.

`brood-deploy` лежит на машине копией (`/usr/local/bin`), её ставит
`provision.sh`. Правка `deploy/brood-deploy.sh` доезжает до машины не
выкладкой, а повторным `provision.sh` либо, от root,
`install -m 0755 deploy/brood-deploy.sh /usr/local/bin/brood-deploy`.

Установка Substrate живёт в каталоге данных пользователя `brood`
(`~brood/.local/share/brood/substrate`), а не в релизе: релиз прошлого
коммита удаляется после удачной выкладки. Два деплоя сразу не идут:
`resource_group` в CI и `flock` на машине.

Защищённые переменные CI проекта (их видят только конвейеры защищённых
веток; `main` защищён):

| переменная | тип | что |
|---|---|---|
| `BROOD_DEPLOY_HOST` | variable | `brood@<адрес машины>` |
| `BROOD_DEPLOY_SSH_KEY` | file | закрытый ключ деплоя (ed25519) |
| `BROOD_DEPLOY_KNOWN_HOSTS` | file | ключ хоста машины деплоя |
| `BROOD_DEPLOY_ENV` | file | `.env` стенда (ниже) |
| `BROOD_DEPLOY_TLS_CERT` | file | сертификат стенда на `brood.corp.ermak.dev` и `brood` |
| `BROOD_DEPLOY_TLS_KEY` | file | его ключ |

Заводит и обновляет их `brood dev ci set` (protected и raw: без raw GitLab
раскрыл бы `$...` внутри значения), смотрит -- `brood dev ci vars`, значений
он не печатает:

```sh
brood dev ci set --file BROOD_DEPLOY_ENV=<stand.env> \
    --file BROOD_DEPLOY_TLS_CERT=<brood.corp.ermak.dev.crt> \
    --file BROOD_DEPLOY_TLS_KEY=<brood.corp.ermak.dev.key>
```

В репозитории нет ни адреса, ни ключа.

## Машина

VM `brood` (VMID 103) на Proxmox `hyper`, по образцу `builder`: Debian 13
genericcloud, cloud-init (пользователь `ermak`, ключ `ermak@laptop`),
`cpu: host` -- внутри есть `/dev/kvm` для micro-VM runtime Substrate. 8 vCPU,
16 ГБ без balloon, диск 100 ГБ на `local-lvm`, сеть `vmbr0` по DHCP.
Vendor-data cloud-init (`nas-iso:snippets/brood-vendor.yaml`) ставит
`qemu-guest-agent`: по нему Proxmox знает адрес машины.

Создана так (на hyper, от root):

```sh
qm create 103 --name brood --memory 16384 --balloon 0 --cores 8 --cpu host \
  --ostype l26 --scsihw virtio-scsi-single --net0 virtio,bridge=vmbr0,firewall=1 \
  --agent enabled=1 --serial0 socket --vga serial0 --onboot 1
qm set 103 --scsi0 local-lvm:0,import-from=<debian-13-genericcloud-amd64.qcow2>,iothread=1
qm resize 103 scsi0 100G
qm set 103 --ide2 local-lvm:cloudinit --boot order=scsi0 --ciuser ermak \
  --sshkeys <keys.pub> --ipconfig0 ip=dhcp --cicustom vendor=nas-iso:snippets/brood-vendor.yaml
qm start 103
qm guest cmd 103 network-get-interfaces    # адрес
```

Образ -- `cloud.debian.org/images/cloud/trixie/latest`, сверенный по
`SHA512SUMS`.

Остальное делает `deploy/provision.sh` (на машине, от root, повтор безопасен):
Docker с buildx, git, python3, Go того toolchain, что требует закреплённый
Substrate, пользователь `brood` в группах `docker` и `kvm`, принудительная
команда и `authorized_keys`, лимиты inotify для kind.

```sh
sudo deploy/provision.sh ci.pub <адрес builder>
```

## Если адрес машины сменился

Адрес выдан DHCP. Сменится -- выкладка падает громко: ключ хоста в
`BROOD_DEPLOY_KNOWN_HOSTS` не сойдётся или машина не ответит; на чужую машину
она не пойдёт. Новый адрес -- `qm guest cmd 103 network-get-interfaces`,
затем `BROOD_DEPLOY_HOST` и `BROOD_DEPLOY_KNOWN_HOSTS` (ключ хоста сверять с
`qm guest exec 103 -- cat /etc/ssh/ssh_host_ed25519_key.pub`, а не брать с
первого соединения).

## Сервер

`brood deploy server` (`brood/dev/deploy`) раскатывает в кластер установки
namespace `brood`:

- `edge` -- вход: nginx с TLS, http -> https, панель на корне
  (`docs/WEB.md`); Service NodePort `30080`/`30443`, kind публикует их на
  порты хоста `BROOD_HTTP_PORT`/`BROOD_HTTPS_PORT`. Отдельным Deployment'ом:
  перекатка Django и прокси не рвёт стримы LLM на входе;
- `django` -- init `migrate` и `ensureadmin`, Django под gunicorn на `:8000`;
  NetworkPolicy пускает только edge (Django верит его `X-Forwarded-Proto`),
  egress -- DNS, postgres, ateapi и точные адреса Kubernetes API (ниже).
  Отдельный ServiceAccount `django` с
  projected-томом: токен с audience `api.ate-system.svc` и ClusterTrustBundle
  signer'а `servicedns.podcert.ate.dev/identity` -- клиент проектов
  (`brood/server/ateapi`, `brood server test` не ходит в сеть: клиент
  подменяется). Метка `ate.dev/api-access=true` на namespace открывает
  NetworkPolicy ate-system к ateapi; принципал -- bootstrap-owner ateapi
  (флаг установки в `brood/dev/substrate/local.py`), поэтому проекты панели
  создают и удаляют atespace;
- `cliproxy` -- CLIProxyAPI из официального образа, состояние (`config.yaml`,
  `auths/`, журналы, плагины) на томе `cliproxy-data`; NetworkPolicy пускает
  только edge;
- `postgres` -- StatefulSet с тем же образом, что bundled Postgres Substrate,
  том 1Gi; NetworkPolicy пускает только Django.

### Пулы воркеров

Выкладка собирает `ateworker-gvisor` из checkout'а установки (`cluster.source`),
не из дерева brood. Вызов соответствует закреплённому Substrate
`4ebd668d62e3f55753765a46579fc6deb81b2f7f`,
`cmd/ate-setup/internal/ko/ko.go`: `hack/run-tool.sh ko build
 go.ate.dev/substrate/cmd/ateworker-gvisor --base-import-paths` с version stamp
через `--ldflags`. Скрипт выбирает pinned Go tool ko, `.ko.yaml` задаёт базовый
образ; как `hack/install-ate-kind.sh`, собираем только `linux/$(go env GOARCH)`.
Образ пушится в реестр установки, результат --
`localhost:5002/ateworker-gvisor@sha256:…`. Кэш Go и `KO_CACHE` в
`<source>/bin/substrate/ko-cache` переживают релизы brood; заданный `KO_CACHE`
сохраняется. Повторный вызов использует стандартные кэши, без собственного
кэша по HEAD, который мог бы пропустить локальные правки исходников.

После сверки владения и read-only discovery API, сборки образов и apply базы
сервер создаёт начальный WorkerPool `brood/brood`: `replicas: 1`,
`sandboxClasses: [{name: gvisor}]`, `workerImage` по digest,
`template.resources.requests: {cpu: 250m, memory: 2Gi}`,
`template.resources.limits: {cpu: "2", memory: 2Gi}`. Это стартовый размер,
не гарантия достаточной памяти для любой нагрузки. Пул не входит в kustomize:
только атомарный `create`, `AlreadyExists` сохраняет существующий объект
(включая гонку с панелью); остальные ошибки останавливают выкладку. Повторная
выкладка не меняет ресурсы, реплики, метаданные или версию образа ни этого,
ни других пулов. Перевод существующего пула на новый digest -- отдельное
осознанное изменение `spec.workerImage`, не побочный эффект выкладки brood.
Контроллер называет Deployment именем пула: команда ждёт появления
`deployment/brood`, затем его rollout, не удаляя пул или воркеры для починки.

Django получает собранную ссылку через `WORKER_POOL_IMAGE` в `brood-env`;
это не пользовательская настройка `.env`. Интеграционный контракт defaults
API -- поле `workerImage` типа string из `WORKER_POOL_IMAGE`, то есть полная
ссылка по digest, а не имя образа или объект. Уже существующие пулы могут
продолжать работать на прежнем digest.

Клиент Kubernetes использует `KUBE_API_URL=https://kubernetes.default.svc`,
`KUBE_API_TOKEN=/var/run/secrets/kubernetes.io/serviceaccount/token` и
`KUBE_API_CA=/var/run/secrets/kubernetes.io/serviceaccount/ca.crt`:
обычный ротируемый токен ServiceAccount, не пользовательский kubeconfig и
не токен ateapi с другим audience. Role/RoleBinding `django-workerpools` в
namespace `brood` дают `django` только `get,list,create,update,delete` для
`ate.dev/workerpools`, без pods, secrets, cluster-wide доступа и `/status`.

Даже namespaced `create WorkerPool` -- сильное право: контроллер создаёт
привилегированные воркеры, а в `brood` также лежат серверные Secret'ы.
Поэтому backend обязан ограничивать опасные поля spec и metadata (включая
выбор ServiceAccount и аннотации), а не проксировать произвольный объект.
Через GUI разрешены только `gvisor` и безопасный образ, ровно digest из
`WORKER_POOL_IMAGE`; пользовательский образ и произвольный runtime запрещены.
RBAC сам по себе этих ограничений не обеспечивает.

До первой записи выкладка читает Service и Endpoints `default/kubernetes`.
Отдельная NetworkPolicy `django-kube-api` разрешает только их адреса
(`/32` или `/128`) и HTTPS-порты: Service на 443 до DNAT, endpoints на их
порту (в kind 6443) после DNAT. Широкого egress на 443 или всю сеть узлов
нет. Запрет чтения, повреждённый ответ или отсутствие готовых endpoints --
объяснимый отказ до сборки и apply, а не расширение политики. При смене
адресов API нужно повторить выкладку, чтобы обновить эту политику.

Маршруты входа:

| путь | куда |
|---|---|
| `/` | панель (один файл) |
| `/admin/`, `/static/`, `/auth/`, `/api/`, `/healthz` | Django |
| `/v1/`, `/v1beta/`, `/openai/v1/`, `/backend-api/codex/` | CLIProxyAPI, клиентский ключ проверяет он сам |
| `/v8/management/` | CLIProxyAPI, только с сессией суперпользователя brood; ключ ставит вход |
| `/{anthropic,codex,antigravity,devin}/callback`, `/callback` | CLIProxyAPI: возвраты OAuth провайдеров |
| прочее | 404, в том числе `/v0/management` и встроенная панель прокси |

Образы brood -- сервер (`brood/dev/deploy/Dockerfile`) и вход с панелью
(`brood/dev/deploy/edge/Dockerfile`) -- собираются и кладутся в реестр
установки (`localhost:5002`) по digest. В конце команда проверяет по https на
`BROOD_HTTPS_PORT` ровно выложенным сертификатом, что отвечают панель,
страница входа админки, ворота management (401 без сессии) и `/v1/models` с
клиентским ключом.

Ключ management прокси -- `MANAGEMENT_PASSWORD` из Secret, а не
`secret-key` в `config.yaml`: конфиг правит панель, и её сохранение ключ не
теряет. Первый `config.yaml` заводит init-контейнер из
`manifests/cliproxy-config.yaml` с клиентским ключом и дальше его не трогает:
`CLIPROXY_CLIENT_KEY` в `.env` после первой выкладки меняет только
проверку выкладки, ключи прокси правятся панелью. Без единого ключа в
`access.api-keys` прокси открыт всем -- последний ключ из панели не удалять.

`.env` стенда -- не `.env` разработчика (в том отладка и личный
`GITLAB_TOKEN`); держите его вне репозитория и загружайте `brood dev ci set`:

| настройка | что |
|---|---|
| `DJANGO_SECRET_KEY` | свой ключ стенда |
| `DJANGO_ALLOWED_HOSTS` | `brood,brood.corp.ermak.dev`; первое имя -- им проверяется вход |
| `POSTGRES_PASSWORD` | пароль базы; читается только при первом initdb, потом -- `ALTER USER` |
| `DJANGO_SUPERUSER_USERNAME`, `DJANGO_SUPERUSER_PASSWORD` | админ: заводится, если его нет, пароль не сбрасывается |
| `DJANGO_SUPERUSER_EMAIL` | необязательно |
| `CLIPROXY_MANAGEMENT_KEY` | ключ management прокси; знают только прокси и вход |
| `CLIPROXY_CLIENT_KEY` | первый клиентский ключ `/v1` |
| `BROOD_HTTPS_PORT`, `BROOD_HTTP_PORT` | `443` и `80`: порты хоста стенда |

В кластер едут только эти ключи: серверу -- свои, учётка админа -- только
init-контейнеру `ensureadmin`, ключи прокси -- прокси и входу. Сменился `.env` или сертификат -- новый
`brood dev ci set` и перезапуск джобы `deploy` (`brood dev ci retry <job>`):
хэш в имени Secret'а перекатит под.

Порты хоста kind задаёт только при создании кластера. Если узел публикует
не то, что в `.env`, `brood substrate install` откажет; пересоздание
кластера стирает его данные, поэтому делается руками, от пользователя
`brood` (ключ выкладки этого не умеет):

```sh
sudo -u brood -i sh -c 'cd releases/current && bin/brood substrate uninstall'
```

и перезапуск джобы `deploy`.
