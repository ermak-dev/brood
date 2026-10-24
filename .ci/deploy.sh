#!/bin/bash
# Выкладка зелёного main на машину деплоя brood (docs/DEPLOY.md), как
# выкладка mop: builder идёт туда по ssh отдельным ключом, и на машине у
# ключа принудительная команда deploy/brood-deploy.sh. Клиент шлёт только sha
# коммита и tar его дерева; что с ними делать, решает машина.
#
# Ключ, known_hosts и адрес -- защищённые переменные CI: их видят только
# конвейеры защищённых веток. В репозитории нет ни хоста, ни ключа.
set -o errexit -o nounset -o pipefail

: "${BROOD_DEPLOY_HOST:?protected CI variable: user@address of the deploy machine}"
: "${BROOD_DEPLOY_SSH_KEY:?protected CI file variable: the deploy key}"
: "${BROOD_DEPLOY_KNOWN_HOSTS:?protected CI file variable: the deploy machine host key}"
: "${CI_COMMIT_SHA:?run by GitLab CI}"

# ssh отвергает ключ, который читают другие: копия в файле mktemp, 0600.
# Перевод строки дописан: без него в конце OpenSSH ключ не читает, а
# файловая переменная GitLab его не гарантирует.
key="$(mktemp)"
trap 'rm -f "$key"' EXIT
{ cat "$BROOD_DEPLOY_SSH_KEY"; echo; } >"$key"

# -F /dev/null: настройки ssh пользователя раннера сюда не доезжают.
git archive --format=tar "$CI_COMMIT_SHA" |
    ssh -F /dev/null -i "$key" -o IdentitiesOnly=yes -o BatchMode=yes \
        -o StrictHostKeyChecking=yes -o UserKnownHostsFile="$BROOD_DEPLOY_KNOWN_HOSTS" \
        -o ServerAliveInterval=30 "$BROOD_DEPLOY_HOST" "$CI_COMMIT_SHA"
