#!/bin/bash
# Выкладка зелёного main на машину деплоя brood (docs/DEPLOY.md), как
# выкладка mop: builder идёт туда по ssh отдельным ключом, и на машине у
# ключа принудительная команда deploy/brood-deploy.sh. Клиент шлёт только sha
# коммита и tar его дерева; что с ними делать, решает машина.
#
# К дереву дописаны закреплённый bootstrap Claude и секреты установки стенда:
# .env и TLS в secrets/, туда, где их ищет рабочая копия.
# Ключ, known_hosts, адрес и секреты -- защищённые
# переменные CI (brood dev ci set): их видят только конвейеры защищённых
# веток. В репозитории нет ни хоста, ни ключа.
set -o errexit -o nounset -o pipefail

: "${BROOD_DEPLOY_HOST:?protected CI variable: user@address of the deploy machine}"
: "${BROOD_DEPLOY_SSH_KEY:?protected CI file variable: the deploy key}"
: "${BROOD_DEPLOY_KNOWN_HOSTS:?protected CI file variable: the deploy machine host key}"
: "${BROOD_DEPLOY_ENV:?protected CI file variable: the stand .env}"
: "${BROOD_DEPLOY_TLS_CERT:?protected CI file variable: the stand TLS certificate}"
: "${BROOD_DEPLOY_TLS_KEY:?protected CI file variable: the stand TLS key}"
: "${CI_COMMIT_SHA:?run by GitLab CI}"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
chmod 0700 "$work"

stage="$work/stage"
bootstrap=.brood-deploy-artifacts
if git cat-file -e "$CI_COMMIT_SHA:$bootstrap" 2>/dev/null; then
    printf '%s\n' 'The release tree already contains the reserved bootstrap path' >&2
    exit 1
fi
read -r version digest < <(git show "$CI_COMMIT_SHA:brood/dev/pins.json" | python3 -I -c \
    'import json,sys; pins=json.load(sys.stdin); print(pins["CLAUDE_VERSION"],pins["CLAUDE_SHA256"])')
if [[ ! $version =~ ^[0-9]+\.[0-9]+\.[0-9]+$ || ! $digest =~ ^[a-f0-9]{64}$ ]]; then
    printf '%s\n' 'Invalid pinned release Claude version or SHA-256' >&2
    exit 1
fi
install -d -m 0700 "$stage/$bootstrap"
claude="$stage/$bootstrap/claude"
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

# Секреты -- под своими именами в рабочей копии, 0600: tar на машине права
# сохранит, а umask их только сужает.
install -d -m 0700 "$stage/secrets"
install -m 0600 "$BROOD_DEPLOY_ENV" "$stage/.env"
install -m 0600 "$BROOD_DEPLOY_TLS_CERT" "$stage/secrets/tls.crt"
install -m 0600 "$BROOD_DEPLOY_TLS_KEY" "$stage/secrets/tls.key"
# Фикстура установки для BROOD_DEPLOY_MODE=fixture: администратор и его Telegram.
if [[ -n ${BROOD_DEPLOY_FIXTURE:-} ]]; then
    install -m 0600 "$BROOD_DEPLOY_FIXTURE" "$stage/secrets/fixture.json"
fi

tree="$work/tree.tar"
git archive --format=tar -o "$tree" "$CI_COMMIT_SHA"
tar --append -f "$tree" -C "$stage" .env secrets "$bootstrap"

# -F /dev/null: настройки ssh пользователя раннера сюда не доезжают.
ssh -F /dev/null -i "$key" -o IdentitiesOnly=yes -o BatchMode=yes \
    -o StrictHostKeyChecking=yes -o UserKnownHostsFile="$BROOD_DEPLOY_KNOWN_HOSTS" \
    -o ServerAliveInterval=30 "$BROOD_DEPLOY_HOST" "$CI_COMMIT_SHA" <"$tree"
