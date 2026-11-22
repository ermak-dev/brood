#!/bin/bash
set -euo pipefail
umask 077
die() { printf '%s\n' "$*" >&2; exit 1; }
[[ $EUID == 0 && $# == 3 ]] || die 'Usage: sudo bash deploy/install-local.sh HOST CERTIFICATE PRIVATE_KEY'
host=$1
[[ $host =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$ ]] || die 'Use a DNS hostname without a scheme, port or path.'
cert=$(realpath -- "$2"); key=$(realpath -- "$3")
openssl x509 -in "$cert" -checkend 0 -noout >/dev/null || die 'Supply a valid, unexpired PEM certificate.'
certpub=$(openssl x509 -in "$cert" -pubkey -noout)
keypub=$(openssl pkey -in "$key" -passin pass: -pubout)
[[ $certpub == "$keypub" ]] || die 'Supply the unencrypted private key matching the certificate.'
cd "$(dirname "$0")/.."
state=/var/lib/brood
unit=/etc/systemd/system/brood.service
client=/usr/local/bin/bro
for path in "$state" "$unit" "$client"; do
    [[ ! -e $path && ! -L $path ]] || die "Nothing installed: $path already exists. Use a fresh host for this installer."
done
if getent passwd brood >/dev/null || getent group brood >/dev/null; then die 'Nothing installed: the brood account or group already exists. Use a fresh host.'; fi
for file in brood bro artifacts/{bro,brood-guest,claude,manifest.json}; do
    [[ -s go/bin/$file ]] || die 'Nothing installed: run bash deploy/build-local.sh first.'
done
work=$(mktemp -d /var/lib/.brood-install.XXXXXXXX)
trap 'rm -rf -- "$work"' EXIT
install -m 0755 go/bin/brood "$work/brood"
cp -r go/bin/artifacts "$work/artifacts"
install -m 0600 "$cert" "$work/tls.crt"
install -m 0600 "$key" "$work/tls.key"
mkdir "$work/gateway"
api=$(openssl rand -hex 32); management=$(openssl rand -hex 32)
session=$(openssl rand -hex 32); relay=$(openssl rand -hex 32)
cat > "$work/.env" <<EOF
BROOD_ADDR=:443
BROOD_PUBLIC_ORIGIN=https://$host
BROOD_DB_PATH=$state/brood.db
BROOD_TLS_CERT=$state/tls.crt
BROOD_TLS_KEY=$state/tls.key
BROOD_ARTIFACTS_DIR=$state/artifacts
BROOD_SESSION_SECRET=$session
BROOD_PROXMOX_RELAY_SECRET=$relay
BROOD_CLIPROXY_CONFIG=$state/gateway/config.yaml
CLIPROXY_API_KEY=$api
CLIPROXY_MANAGEMENT_KEY=$management
EOF
cat > "$work/gateway/config.yaml" <<EOF
config-version: 8
server:
  host: 127.0.0.1
  port: 8317
access:
  api-keys: ['$api']
management:
  allow-remote: false
  secret-key: '$management'
  disable-control-panel: true
oauth:
  auth-dir: $state/gateway/auth
EOF
useradd --system --user-group --home-dir "$state" --no-create-home --shell /usr/sbin/nologin brood
chown -R brood:brood "$work"
mv -T "$work" "$state"
install -D -m 0755 go/bin/bro "$client"
install -m 0644 deploy/brood.service "$unit"
systemctl daemon-reload
systemctl enable --now brood.service
for ((i=0; i<30; i++)); do
    if [[ $(curl --silent --fail --max-time 2 --noproxy '*' --resolve "$host:443:127.0.0.1" --cacert "$state/tls.crt" "https://$host/healthz") == ok ]]; then
        printf 'Server ready at https://%s. Create the first administrator as described in the README.\n' "$host"
        exit 0
    fi
    sleep 1
done
die 'Files installed, but HTTPS is not ready. Check the hostname, certificate and journalctl -u brood.service.'
