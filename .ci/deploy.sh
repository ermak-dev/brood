#!/bin/bash
# Builder собирает программы и передаёт их машине деплоя через ключ с
# принудительной командой. Секреты добавляются после сборки, вне её контекста.
set +x
set -o errexit -o nounset -o pipefail
umask 077

: "${BROOD_DEPLOY_HOST:?protected CI variable: user@address of the deploy machine}"
: "${BROOD_DEPLOY_SSH_KEY:?protected CI file variable: the deploy key}"
: "${BROOD_DEPLOY_KNOWN_HOSTS:?protected CI file variable: the deploy machine host key}"
: "${BROOD_DEPLOY_ENV:?protected CI file variable: the stand .env}"
: "${BROOD_DEPLOY_TLS_CERT:?protected CI file variable: the stand TLS certificate}"
: "${BROOD_DEPLOY_TLS_KEY:?protected CI file variable: the stand TLS key}"
: "${CI_COMMIT_SHA:?run by GitLab CI}"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

[[ $CI_COMMIT_SHA =~ ^[0-9a-f]{40}$ ]] || { echo 'Expected a full commit SHA' >&2; exit 1; }

stage="$work/stage"
install -d "$stage/bin" "$work/src"
git archive "$CI_COMMIT_SHA" | tar -x -C "$work/src"
docker buildx build --platform linux/amd64 --file "$work/src/deploy/Dockerfile.go" \
    --target binaries --build-arg "VERSION=$CI_COMMIT_SHA" --output "type=local,dest=$stage/bin" "$work/src"

read -r version digest < <(git show "$CI_COMMIT_SHA:brood/dev/pins.json" | python3 -I -c \
    'import json,sys; pins=json.load(sys.stdin); print(pins["CLAUDE_VERSION"],pins["CLAUDE_SHA256"])')
if [[ ! $version =~ ^[0-9]+\.[0-9]+\.[0-9]+$ || ! $digest =~ ^[a-f0-9]{64}$ ]]; then
    printf '%s\n' 'Invalid pinned release Claude version or SHA-256' >&2
    exit 1
fi
claude="$stage/bin/claude"
cache_dir="$PWD/.brood-ci-cache"
cached="$cache_dir/claude"
max_bytes=536870912
verify_claude() { printf '%s  %s\n' "$digest" "$claude" | sha256sum --check --status; }
# Кеш недоверенный: проверяем приватную копию, не передаём исходный файл.
if [[ -d $cache_dir && ! -L $cache_dir && -f $cached && ! -L $cached ]] &&
        [[ $(stat -c %s -- "$cached") -le $max_bytes ]] &&
        cp -- "$cached" "$claude" && verify_claude; then
    printf '%s\n' 'Using verified builder bootstrap Claude cache'
else
    rm -f -- "$claude"
    # Публичная загрузка не получает ни токен CI, ни настройки выкладки.
    for attempt in 1 2 3 4; do
        if env -i PATH=/usr/bin:/bin curl --disable -fsSL --proto '=https' --proto-redir '=https' \
            --connect-timeout 20 --max-time 150 --max-filesize "$max_bytes" --continue-at - -o "$claude" \
            "https://downloads.claude.ai/claude-code-releases/$version/linux-x64/claude"; then
            break
        else
            code=$?
        fi
        if (( code != 28 || attempt == 4 )); then
            printf 'Builder bootstrap Claude download failed (curl exit %s)\n' "$code" >&2
            exit 1
        fi
    done
    if ! verify_claude; then
        printf '%s\n' 'Builder bootstrap Claude artifact SHA-256 mismatch' >&2
        exit 1
    fi
    # Кеширование не является условием выкладки. Сохраняем полные байты атомарно.
    if [[ ! -L $cache_dir ]] && mkdir -p -- "$cache_dir" && partial=$(mktemp "$cache_dir/.claude.XXXXXX"); then
        if ! cp -- "$claude" "$partial" || ! mv --no-target-directory -- "$partial" "$cached"; then
            rm -f -- "$partial"
        fi
    fi
fi
chmod 0600 "$claude"
printf '%s\n' 'Pinned bootstrap Claude verified on builder'

# ssh отвергает ключ, который читают другие: копия в файле 0600.
# Перевод строки дописан: без него в конце OpenSSH ключ не читает, а
# файловая переменная GitLab его не гарантирует.
key="$work/key"
{ cat "$BROOD_DEPLOY_SSH_KEY"; echo; } >"$key"
chmod 0600 "$key"

# Секреты не входят ни в контекст сборки, ни в артефакты GitLab.
install -d -m 0700 "$stage/secrets"
install -m 0600 "$BROOD_DEPLOY_ENV" "$stage/.env"
install -m 0600 "$BROOD_DEPLOY_TLS_CERT" "$stage/secrets/tls.crt"
install -m 0600 "$BROOD_DEPLOY_TLS_KEY" "$stage/secrets/tls.key"
# Фикстура установки для BROOD_DEPLOY_MODE=fixture: администратор и его Telegram.
if [[ -n ${BROOD_DEPLOY_FIXTURE:-} ]]; then
    install -m 0600 "$BROOD_DEPLOY_FIXTURE" "$stage/secrets/fixture.json"
fi

# -F /dev/null: настройки ssh пользователя раннера сюда не доезжают.
tar -c -C "$stage" .env bin secrets | ssh -F /dev/null -i "$key" -o IdentitiesOnly=yes -o BatchMode=yes \
    -o StrictHostKeyChecking=yes -o UserKnownHostsFile="$BROOD_DEPLOY_KNOWN_HOSTS" \
    -o ServerAliveInterval=30 -- "$BROOD_DEPLOY_HOST" "$CI_COMMIT_SHA"
