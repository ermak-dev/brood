#!/bin/bash
# Ключ CI передаёт sha в SSH_ORIGINAL_COMMAND, tar — на stdin. При запуске
# из systemd та же программа читает настройки уже от имени пользователя brood.
set +x
set -o errexit -o nounset -o pipefail
umask 077
export PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C.UTF-8

state=/var/lib/brood
export HOME="$state"
cd -- "$state"
releases="$state/releases"
unit=brood.service
fail() { printf 'brood-deploy: %s\n' "$*" >&2; exit 1; }

trim() {
    REPLY="${1#"${1%%[![:space:]]*}"}"
    REPLY="${REPLY%"${REPLY##*[![:space:]]}"}"
}

settings() {
    local line name value REPLY
    local dir=$1
    [[ -r $dir/.env ]] || fail 'Release .env is missing'
    # .env — данные, а не команды. Go сам снимает внешние кавычки.
    while IFS= read -r line || [[ -n $line ]]; do
        trim "$line"
        line=$REPLY
        [[ $line != \#* && $line == *=* ]] || continue
        trim "${line%%=*}"
        name=$REPLY
        [[ $name =~ ^(BROOD|CLIPROXY)_[A-Z0-9_]+$ ]] || continue
        trim "${line#*=}"
        value=$REPLY
        case "$name" in
            BROOD_PUBLIC_HOST|BROOD_HTTPS_PORT|BROOD_DEPLOY_MODE)
                case "$value" in
                    \"*\") value=${value:1:-1} ;;
                    \'*\') value=${value:1:-1} ;;
                esac
                ;;
        esac
        export "$name=$value"
    done <"$dir/.env"

    [[ ${BROOD_PUBLIC_HOST:-} =~ ^[a-zA-Z0-9]([a-zA-Z0-9.-]*[a-zA-Z0-9])?$ ]] ||
        fail 'Set BROOD_PUBLIC_HOST to the public DNS name in .env'
    [[ ${BROOD_HTTPS_PORT:-} =~ ^[1-9][0-9]{0,4}$ ]] && (( BROOD_HTTPS_PORT <= 65535 )) ||
        fail 'Set BROOD_HTTPS_PORT to a port from 1 to 65535 in .env'
    export BROOD_ADDR=":$BROOD_HTTPS_PORT"
    export BROOD_DB_PATH="$state/brood.db"
    export BROOD_ARTIFACTS_DIR="$dir/artifacts"
    export BROOD_TLS_CERT="$dir/secrets/tls.crt" BROOD_TLS_KEY="$dir/secrets/tls.key"
    unset BROOD_DB_RESET BROOD_FIXTURE
    case "${BROOD_DEPLOY_MODE:-existing}" in
        existing) ;;
        fixture)
            local fixture="$dir/secrets/fixture.json"
            [[ -s $fixture ]] || fail 'Fixture mode requires protected CI file BROOD_DEPLOY_FIXTURE'
            # Один аргумент окружения на Linux ограничен размером 32 страниц.
            (( $(stat -c %s -- "$fixture") <= 120000 )) || fail 'Installation fixture exceeds 120000 bytes'
            export BROOD_DB_RESET="${dir##*/}"
            BROOD_DB_RESET="${BROOD_DB_RESET%%.*}"
            BROOD_FIXTURE=$(<"$fixture")
            ;;
        *) fail 'Set BROOD_DEPLOY_MODE to existing or fixture in .env' ;;
    esac
}

if [[ ${1:-} == serve && $# -eq 1 ]]; then
    release=$(readlink -e "$releases/current") || fail 'No release is selected'
    [[ $release == "$releases/"* && ${release##*/} =~ ^[0-9a-f]{40}\.[a-zA-Z0-9]+$ ]] ||
        fail 'The selected release path is invalid'
    settings "$release"
    if [[ -n ${BROOD_DB_RESET:-} ]]; then
        export BROOD_FIXTURE
    fi
    exec "$release/bin/brood" serve
fi

sha=${SSH_ORIGINAL_COMMAND:-}
[[ $# -eq 0 && $sha =~ ^[0-9a-f]{40}$ ]] || fail 'Expected a full commit SHA'

mkdir -p "$releases"
exec 9>"$releases/.lock"
flock -n 9 || fail 'Another deploy is running; retry after it finishes'

release=$(mktemp -d "$releases/$sha.XXXXXXXX")
cleanup() {
    if [[ $(readlink "$releases/current" 2>/dev/null || :) != "$release" ]]; then
        rm -rf -- "$release"
    fi
}
trap cleanup EXIT

# Формат закрытый: только перечисленные обычные файлы и каталоги bin/secrets.
# Права и пути из tar не используются, ссылки и повторные записи запрещены.
python3 -I -c '
import pathlib, shutil, sys, tarfile

root = pathlib.Path(sys.argv[1])
limits = {
    ".env": 1 << 20, "secrets/tls.crt": 1 << 20, "secrets/tls.key": 1 << 20,
    "secrets/fixture.json": 120000,
    **{f"bin/{name}": 512 << 20 for name in ("brood", "bro", "brood-guest", "claude")},
}
seen = set()
try:
    for directory in ("bin", "secrets"):
        (root / directory).mkdir()
    with tarfile.open(fileobj=sys.stdin.buffer, mode="r|") as archive:
        for member in archive:
            name = member.name
            if member.isdir() and name in ("bin", "secrets"):
                continue
            if (member.type not in (tarfile.REGTYPE, tarfile.AREGTYPE) or name not in limits
                    or name in seen or not 0 < member.size <= limits[name]):
                raise ValueError
            with archive.extractfile(member) as data, (root / name).open("xb") as output:
                shutil.copyfileobj(data, output)
            seen.add(name)
    if set(limits) - {"secrets/fixture.json"} - seen:
        raise ValueError
    # Дочитываем концевые блоки tar, чтобы отправитель не получил SIGPIPE.
    while sys.stdin.buffer.read(65536):
        pass
except (ValueError, OSError, tarfile.TarError):
    sys.exit("brood-deploy: invalid or incomplete release archive; active release unchanged")
' "$release"

settings "$release"
chmod 0700 "$release/bin/brood"
if ! openssl x509 -in "$BROOD_TLS_CERT" -checkend 0 -noout >/dev/null 2>&1 ||
        ! cert_pub=$(openssl x509 -in "$BROOD_TLS_CERT" -pubkey -noout 2>/dev/null) ||
        ! key_pub=$(openssl pkey -in "$BROOD_TLS_KEY" -passin pass: -pubout 2>/dev/null) ||
        [[ $cert_pub != "$key_pub" ]]; then
    fail 'Invalid, expired or mismatched TLS files; active release unchanged'
fi
if [[ -n ${BROOD_DB_RESET:-} ]]; then
    # Проверяем фикстуру и конфигурацию до переключения, не трогая рабочую БД.
    if ! BROOD_DB_PATH="$release/fixture-check.db" "$release/bin/brood" fixture apply \
            <"$release/secrets/fixture.json" >/dev/null 2>&1; then
        fail 'Could not apply fixture to a fresh database; check fixture and .env; active release unchanged'
    fi
    rm -f -- "$release"/fixture-check.db*
fi

{
    for name in bro brood-guest claude; do
        file="$release/bin/$name"
        size=$(stat -c %s -- "$file") || exit 1
        printf '%s\0%s\0' "$name" "$size"
        cat -- "$file" || exit 1
    done
} | "$release/bin/brood" artifacts --dir "$BROOD_ARTIFACTS_DIR" >/dev/null
rm -- "$release/bin/"{bro,brood-guest,claude}

# Используем неподвижный каталог релиза и атомарно меняем только ссылку.
# После попытки старта не откатываем поколение базы: fixture уже мог сработать.
ln -s -- "$release" "$release/.next"
mv -Tf -- "$release/.next" "$releases/current"
if ! sudo -n -- /usr/bin/systemctl restart "$unit"; then
    fail 'Release selected but systemd could not restart brood.service; inspect the host journal'
fi

for ((attempt=1; attempt<=30; attempt++)); do
    if response=$(curl --disable --fail --silent --max-time 2 --noproxy '*' \
        --resolve "$BROOD_PUBLIC_HOST:$BROOD_HTTPS_PORT:127.0.0.1" --cacert "$BROOD_TLS_CERT" \
        --max-filesize 256 --write-out '%{http_code}' \
        "https://$BROOD_PUBLIC_HOST:$BROOD_HTTPS_PORT/healthz") && [[ $response == $'ok\n200' ]]; then
        find "$releases" -mindepth 1 -maxdepth 1 -type d ! -path "$release" -exec rm -rf -- {} +
        printf 'brood-deploy: %s is healthy over HTTPS\n' "$sha"
        exit 0
    fi
    sleep 1
done
fail 'Release selected but HTTPS /healthz did not become ready; inspect the host journal and TLS settings'
