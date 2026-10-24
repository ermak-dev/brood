#!/bin/bash
# Машина деплоя brood (docs/DEPLOY.md): то, что нужно `brood substrate
# install`, и вход для выкладки из CI, на чистом Debian 13. Запускается на
# ней от root из каталога deploy/ рабочей копии, повторный запуск ничего не
# ломает:
#
#   provision.sh KEY FROM
#
# KEY -- файл с открытым ключом деплоя (его закрытая половина -- переменная
# CI), FROM -- адрес builder: ключ принимается только оттуда и умеет только
# принудительную команду brood-deploy.
#
# Go закреплён по контрольной сумме, как образы по digest: это toolchain,
# которого требует go.mod закреплённого Substrate (Debian собирает свой go с
# GOTOOLCHAIN=local, и другой он не скачает).
set -o errexit -o nounset -o pipefail

GO_VERSION="1.27.3"
GO_SHA256="65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40"
DEPLOY_USER="brood"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [[ $# -ne 2 ]]; then
    echo "usage: provision.sh KEY FROM" >&2
    exit 2
fi
# Ровно одна строка: вторая легла бы в authorized_keys без restrict, то есть
# ключом с полным шеллом.
read -r KEY <"$1"
if [[ ! "$KEY" =~ ^ssh-ed25519\ [A-Za-z0-9+/=]+(\ [^[:space:]]+)?$ ]] ||
        [[ $(grep -c . "$1") -ne 1 ]]; then
    echo "provision.sh: $1 must hold exactly one ssh-ed25519 public key" >&2
    exit 2
fi
FROM="$2"
if [[ $(id -u) -ne 0 ]]; then
    echo "provision.sh: run as root" >&2
    exit 1
fi

export DEBIAN_FRONTEND=noninteractive
# LC_* оператора, приехавшие по ssh, на машине не сгенерированы.
export LC_ALL=C.UTF-8
apt-get update -qq
# В Debian 13 клиент docker и buildx -- отдельные пакеты; buildx нужен
# `docker build --platform`, которым Substrate собирает образ envoy.
apt-get install -y -qq --no-install-recommends ca-certificates curl git python3 \
    docker.io docker-cli docker-buildx >/dev/null

if [[ "$(/usr/local/go/bin/go version 2>/dev/null)" != "go version go${GO_VERSION} "* ]]; then
    tmp="$(mktemp)"
    curl -fsSL -o "$tmp" "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz"
    if ! echo "${GO_SHA256}  ${tmp}" | sha256sum -c --status; then
        rm -f "$tmp"
        echo "provision.sh: checksum mismatch for go${GO_VERSION}" >&2
        exit 1
    fi
    rm -rf /usr/local/go
    tar -C /usr/local -xzf "$tmp"
    rm -f "$tmp"
fi
# В /usr/local/bin, а не в профиле: команда ssh и login-шелл видят его одинаково.
ln -sf /usr/local/go/bin/go /usr/local/bin/go
ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt

# Деплой -- от непривилегированного пользователя. docker -- им kind
# поднимает кластер; kvm -- micro-VM runtime Substrate.
if ! id "$DEPLOY_USER" >/dev/null 2>&1; then
    useradd --create-home --shell /bin/bash "$DEPLOY_USER"
fi
usermod -a -G docker,kvm "$DEPLOY_USER"
install -m 0755 "$HERE/brood-deploy.sh" /usr/local/bin/brood-deploy
home="$(getent passwd "$DEPLOY_USER" | cut -d: -f6)"
install -d -m 0700 -o "$DEPLOY_USER" -g "$DEPLOY_USER" "$home/.ssh"
printf 'restrict,from="%s",command="/usr/local/bin/brood-deploy" %s\n' "$FROM" "$KEY" |
    install -m 0600 -o "$DEPLOY_USER" -g "$DEPLOY_USER" /dev/stdin "$home/.ssh/authorized_keys"

# kind: узел кластера -- контейнер, и стандартных лимитов inotify ему не
# хватает (kind, known issues: too many open files).
cat >/etc/sysctl.d/90-brood-kind.conf <<'EOF'
fs.inotify.max_user_watches = 524288
fs.inotify.max_user_instances = 512
EOF
sysctl -q --system

systemctl enable --now docker >/dev/null
echo "provisioned: $(go version), $(docker --version), $(docker buildx version | cut -d' ' -f1,2)"
