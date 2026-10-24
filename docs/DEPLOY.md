# DEPLOY

Зелёный `main` раскатывается сам: джоба `deploy` пайплайна ставит на машину
деплоя Substrate тем же `brood substrate install`, что у разработчика
(`docs/SUBSTRATE.md`), затем `brood substrate start`.

## Как идёт выкладка

Джоба идёт на раннере `builder` (shell, тег `docker`), как выкладка mop:

1. `.ci/deploy.sh` шлёт по ssh `git archive` коммита и его sha пользователю
   `brood` машины деплоя;
2. в `authorized_keys` у этого ключа
   `restrict,from="<builder>",command="/usr/local/bin/brood-deploy"`: клиент
   никакой команды не выбирает, ключ принимается только с builder и умеет
   только выкладку;
3. `brood-deploy` (`deploy/brood-deploy.sh`) распаковывает дерево в
   `~brood/releases/<sha>` и зовёт из него `bin/brood substrate install` и
   `bin/brood substrate start`. Вывод идёт в лог джобы, код выхода -- её код.

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
