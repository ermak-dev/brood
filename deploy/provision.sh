#!/bin/bash
# Запускается оператором от root на Debian: provision.sh KEY FROM.
# KEY — один открытый ключ CI; FROM — адрес или CIDR сборщика.
set -o errexit -o nounset -o pipefail

user=brood
here="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

if [[ $# -ne 2 ]]; then
    echo 'usage: provision.sh KEY FROM' >&2
    exit 2
fi
# Только одна строка ключа, без опций клиента и произвольного комментария.
mapfile -t keys <"$1"
if [[ ${#keys[@]} -ne 1 || ! ${keys[0]} =~ ^ssh-ed25519\ [A-Za-z0-9+/=]+(\ [^[:cntrl:]]+)?$ ]]; then
    echo 'provision.sh: KEY must contain exactly one ssh-ed25519 public key' >&2
    exit 2
fi
from=$2
if [[ ! $from =~ ^[a-zA-Z0-9.:/_-]+$ ]]; then
    echo 'provision.sh: FROM must be the builder address or CIDR' >&2
    exit 2
fi
if [[ $(id -u) -ne 0 || $(dpkg --print-architecture) != amd64 ]]; then
    echo 'provision.sh: run as root on Debian amd64' >&2
    exit 1
fi

export DEBIAN_FRONTEND=noninteractive LC_ALL=C.UTF-8
apt-get update -qq
apt-get install -y -qq --no-install-recommends ca-certificates curl openssl openssh-server python3 sudo >/dev/null

if ! id "$user" >/dev/null 2>&1; then
    useradd --create-home --user-group --shell /bin/bash "$user"
fi
# В том числе убираем старые группы docker и kvm: у ключа нет других прав.
usermod -G '' "$user"
home=$(getent passwd "$user" | cut -d: -f6)
# Пользователь сервиса не может заменить принудительную команду или ключи SSH.
# Данные сервиса и его HOME находятся отдельно, в /var/lib/brood.
chown root:root "$home"
chmod 0755 "$home"
# Bash под ssh может прочитать ~/.bashrc до запуска forced command.
rm -f -- "$home/.bashrc"
install -m 0644 -o root -g root /dev/null "$home/.bashrc"
if [[ -L $home/.ssh || -L $home/.ssh/authorized_keys ]]; then
    echo 'provision.sh: remove SSH configuration symlinks before proceeding' >&2
    exit 1
fi
install -d -m 0700 -o root -g root "$home/.ssh"
printf 'restrict,from="%s",command="/usr/local/bin/brood-deploy" %s\n' "$from" "${keys[0]}" |
    install -m 0600 -o root -g root /dev/stdin "$home/.ssh/authorized_keys"
# sshd открывает authorized_keys от имени пользователя: читать можно,
# заменять файлы и каталоги нельзя.
chmod 0755 "$home/.ssh"
chmod 0644 "$home/.ssh/authorized_keys"

install -d -m 0700 -o "$user" -g "$user" /var/lib/brood
install -m 0755 "$here/brood-deploy.sh" /usr/local/bin/brood-deploy
install -m 0644 "$here/brood.service" /etc/systemd/system/brood.service

rule=$(mktemp)
trap 'rm -f -- "$rule"' EXIT
printf '%s ALL=(root) NOPASSWD: /usr/bin/systemctl restart brood.service\n' "$user" >"$rule"
visudo -cf "$rule" >/dev/null
install -m 0440 "$rule" /etc/sudoers.d/brood-deploy

systemctl daemon-reload
systemctl enable brood.service
printf '%s\n' 'Provisioned brood.service and the restricted deploy key; no release started'
