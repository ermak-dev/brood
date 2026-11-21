#!/bin/bash
# Ключ CI передаёт sha в SSH_ORIGINAL_COMMAND, tar — на stdin.
set +x
set -o errexit -o nounset -o pipefail
umask 077
export PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C.UTF-8

state=/var/lib/brood
fail() { printf 'brood-deploy: %s\n' "$*" >&2; exit 1; }

sha=${SSH_ORIGINAL_COMMAND:-}
[[ $# -eq 0 && $sha =~ ^[0-9a-f]{40}$ ]] || fail 'Expected a full commit SHA'

export HOME="$state"
cd -- "$state"
exec 9>"$state/.deploy.lock"
flock -n 9 || fail 'Another deploy is running; retry after it finishes'

# После прерванной выкладки следующий запуск уберёт незавершённую подготовку.
stage="$state/.deploy"
rm -rf -- "$stage"
mkdir -- "$stage"
trap 'rm -rf -- "$stage"' EXIT
trap 'exit 1' HUP INT TERM

# Принимаем только обычные заголовки tar без расширений, ссылок и повторов.
# Имена и размеры проверяем до записи; права и владельцев из tar не берём.
python3 -I -c '
import pathlib, sys, tarfile

root = pathlib.Path(sys.argv[1])
limits = {
    ".env": 1 << 20, "secrets/tls.crt": 1 << 20, "secrets/tls.key": 1 << 20,
    "secrets/fixture.json": 1 << 20,
    **{f"bin/{name}": 512 << 20 for name in ("brood", "bro", "brood-guest", "claude")},
}
directories = {"bin", "secrets"}
seen = set()
stream = sys.stdin.buffer
block = tarfile.BLOCKSIZE
zero = bytes(block)
try:
    for directory in directories:
        (root / directory).mkdir()
    while (header := stream.read(block)) != zero:
        member = tarfile.TarInfo.frombuf(header, "utf-8", "strict")
        name = member.name
        if name in seen:
            raise ValueError
        seen.add(name)
        if member.type == tarfile.DIRTYPE and name in directories and member.size == 0:
            continue
        if (member.type not in (tarfile.REGTYPE, tarfile.AREGTYPE) or name not in limits
                or not 0 < member.size <= limits[name]):
            raise ValueError
        with (root / name).open("xb") as output:
            remaining = member.size
            while remaining:
                data = stream.read(min(remaining, 1 << 20))
                if not data:
                    raise ValueError
                output.write(data)
                remaining -= len(data)
        padding = -member.size % block
        if stream.read(padding) != bytes(padding):
            raise ValueError
    if set(limits) - {"secrets/fixture.json"} - seen or stream.read(block) != zero:
        raise ValueError
    while chunk := stream.read(65536):
        if chunk.strip(b"\0"):
            raise ValueError
except (ValueError, OSError, tarfile.TarError):
    sys.exit("brood-deploy: invalid or incomplete deploy archive; installation unchanged")
' "$stage"

cert="$stage/secrets/tls.crt"
key="$stage/secrets/tls.key"
if ! openssl x509 -in "$cert" -checkend 0 -noout >/dev/null 2>&1 ||
        ! cert_pub=$(openssl x509 -in "$cert" -pubkey -noout 2>/dev/null) ||
        ! key_pub=$(openssl pkey -in "$key" -passin pass: -pubout 2>/dev/null) ||
        [[ $cert_pub != "$key_pub" ]]; then
    fail 'Invalid, expired or mismatched TLS files; installation unchanged'
fi
chmod 0700 "$stage/bin/brood"

# .env читает процесс Brood без прав root, не systemd и не оболочка.
config=$(python3 -I -c '
import os, pathlib, re, subprocess, sys

state, stage = map(pathlib.Path, sys.argv[1:3])
sha = sys.argv[3]

def fail(message):
    sys.exit(f"brood-deploy: {message}; installation unchanged")

def unquote(value):
    if len(value) >= 2 and value[0] in (chr(34), chr(39)) and value[-1] == value[0]:
        return value[1:-1]
    return value

try:
    text = (stage / ".env").read_text(encoding="utf-8")
    if any(c == "\0" or ord(c) == 0xFEFF or 0xFDD0 <= ord(c) <= 0xFDEF
           or (ord(c) & 0xFFFF) >= 0xFFFE for c in text):
        raise ValueError
except (OSError, ValueError):
    fail("Use UTF-8 text for the CI .env")

env = {}
for line in text.split("\n"):
    name, sep, value = line.partition("=")
    name = name.strip()
    if sep and re.fullmatch(r"(?:BROOD|CLIPROXY)_[A-Z0-9_]+", name):
        env[name] = value.strip()

host = unquote(env.get("BROOD_PUBLIC_HOST", ""))
port = unquote(env.get("BROOD_HTTPS_PORT", ""))
mode = unquote(env.get("BROOD_DEPLOY_MODE", "")) or "existing"
if not re.fullmatch(r"[a-zA-Z0-9](?:[a-zA-Z0-9.-]*[a-zA-Z0-9])?", host):
    fail("Set BROOD_PUBLIC_HOST to the public DNS name in CI .env")
if not re.fullmatch(r"[1-9][0-9]{0,4}", port) or int(port) > 65535:
    fail("Set BROOD_HTTPS_PORT to a port from 1 to 65535 in CI .env")
if mode not in ("existing", "fixture"):
    fail("Set BROOD_DEPLOY_MODE to existing or fixture in CI .env")

for name in ("BROOD_DB_RESET", "BROOD_FIXTURE", "BROOD_FIXTURE_FILE"):
    env.pop(name, None)
env.update(BROOD_ADDR=f":{port}", BROOD_DB_PATH=str(state / "brood.db"),
           BROOD_ARTIFACTS_DIR=str(state / "artifacts"),
           BROOD_TLS_CERT=str(state / "tls.crt"), BROOD_TLS_KEY=str(state / "tls.key"))
if mode == "fixture":
    fixture = stage / "secrets/fixture.json"
    if not fixture.is_file():
        fail("Fixture mode requires protected CI file BROOD_DEPLOY_FIXTURE")
    env.update(BROOD_DB_RESET=sha, BROOD_FIXTURE_FILE=str(state / "fixture.json"))
    # Проверяем новую конфигурацию и фикстуру в отдельной базе до записи в установку.
    preflight = dict(env, PATH=os.environ["PATH"], HOME=str(state), LC_ALL=os.environ["LC_ALL"],
                     BROOD_DB_PATH=str(stage / "fixture-check.db"),
                     BROOD_TLS_CERT=str(stage / "secrets/tls.crt"),
                     BROOD_TLS_KEY=str(stage / "secrets/tls.key"), BROOD_FIXTURE_FILE=str(fixture))
    try:
        with fixture.open("rb") as data:
            subprocess.run([stage / "bin/brood", "fixture", "apply"], env=preflight, stdin=data,
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True)
    except (OSError, subprocess.CalledProcessError):
        fail("Could not apply fixture to a fresh database; check fixture and CI .env")

try:
    with (stage / "runtime.env").open("x", encoding="utf-8") as output:
        for name, value in env.items():
            output.write(f"{name}={value}\n")
except OSError:
    fail("Could not prepare the runtime .env; check available disk space")
print(host, port, mode)
' "$state" "$stage" "$sha")
read -r host port mode <<<"$config"

if ! {
    for name in bro brood-guest claude; do
        file="$stage/bin/$name"
        size=$(stat -c %s -- "$file") || exit 1
        printf '%s\0%s\0' "$name" "$size"
        cat -- "$file" || exit 1
    done
} | "$stage/bin/brood" artifacts --dir "$stage/artifacts" >/dev/null; then
    fail 'Could not prepare agent artifacts; installation unchanged'
fi

# Файлы меняются переименованием, .env публикуется последним. Каталог
# артефактов меняем целиком, чтобы не оставлять слушатель без каталога.
python3 -I -c '
import ctypes, os, pathlib, sys

state, stage = map(pathlib.Path, sys.argv[1:3])
mode = sys.argv[3]
try:
    src, dst = stage / "artifacts", state / "artifacts"
    if os.path.lexists(dst):
        # Linux renameat2(RENAME_EXCHANGE) обменивает непустые каталоги.
        at_fdcwd, rename_exchange = -100, 2
        libc = ctypes.CDLL(None, use_errno=True)
        if libc.renameat2(at_fdcwd, os.fsencode(src), at_fdcwd, os.fsencode(dst), rename_exchange) != 0:
            raise OSError(ctypes.get_errno())
    else:
        os.rename(src, dst)
    for source, name in (("bin/brood", "brood"), ("secrets/tls.crt", "tls.crt"),
                         ("secrets/tls.key", "tls.key")):
        os.replace(stage / source, state / name)
    if mode == "fixture":
        os.replace(stage / "secrets/fixture.json", state / "fixture.json")
    os.replace(stage / "runtime.env", state / ".env")
    if mode == "existing":
        (state / "fixture.json").unlink(missing_ok=True)
except (OSError, AttributeError):
    sys.exit("brood-deploy: could not install prepared files; installation may be partially updated; check the state directory and retry")
' "$state" "$stage" "$mode"
rm -rf -- "$stage" || fail 'Files installed but temporary deploy files could not be removed; check the state directory and retry'

if ! sudo -n -- /usr/bin/systemctl restart brood.service; then
    fail 'Files installed but systemd could not restart brood.service; inspect the host journal'
fi

for ((attempt=1; attempt<=30; attempt++)); do
    if response=$(curl --disable --fail --silent --max-time 2 --noproxy '*' \
        --resolve "$host:$port:127.0.0.1" --cacert "$state/tls.crt" \
        --max-filesize 256 --write-out '%{http_code}' \
        "https://$host:$port/healthz") && [[ $response == $'ok\n200' ]]; then
        printf 'brood-deploy: %s is healthy over HTTPS\n' "$sha"
        exit 0
    fi
    sleep 1
done
fail 'Files installed but HTTPS /healthz did not become ready; inspect the host journal and TLS settings'
