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

- `postgres` -- StatefulSet с тем же образом, что bundled Postgres Substrate,
  том 1Gi; NetworkPolicy пускает к нему только сервер;
- `brood-server` -- под из init `migrate` и `ensureadmin`, Django под
  gunicorn на `127.0.0.1:8000` и nginx перед ним: TLS, http -> https;
- Service NodePort `30080`/`30443`; kind публикует их на порты хоста
  `BROOD_HTTP_PORT`/`BROOD_HTTPS_PORT`.

Образ собирается из `brood/dev/deploy/Dockerfile` и кладётся в реестр
установки (`localhost:5002`) по digest. В конце команда проверяет, что
страница входа админки отвечает по https на `BROOD_HTTPS_PORT` и отдаёт
ровно выложенный сертификат.

`.env` стенда -- не `.env` разработчика (в том отладка и личный
`GITLAB_TOKEN`); держите его вне репозитория и загружайте `brood dev ci set`:

| настройка | что |
|---|---|
| `DJANGO_SECRET_KEY` | свой ключ стенда |
| `DJANGO_ALLOWED_HOSTS` | `brood,brood.corp.ermak.dev`; первое имя -- им проверяется вход |
| `POSTGRES_PASSWORD` | пароль базы; читается только при первом initdb, потом -- `ALTER USER` |
| `DJANGO_SUPERUSER_USERNAME`, `DJANGO_SUPERUSER_PASSWORD` | админ: заводится, если его нет, пароль не сбрасывается |
| `DJANGO_SUPERUSER_EMAIL` | необязательно |
| `BROOD_HTTPS_PORT`, `BROOD_HTTP_PORT` | `443` и `80`: порты хоста стенда |

В кластер едут только эти ключи: серверу -- свои, учётка админа -- только
init-контейнеру `ensureadmin`. Сменился `.env` или сертификат -- новый
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
