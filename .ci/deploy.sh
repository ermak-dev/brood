#!/bin/bash
# Выкладка зелёного main на машину деплоя brood (docs/DEPLOY.md), как
# выкладка mop: builder идёт туда по ssh отдельным ключом, и на машине у
# ключа принудительная команда deploy/brood-deploy.sh. Клиент шлёт только sha
# коммита и tar его дерева; что с ними делать, решает машина.
#
# К дереву дописаны секреты установки стенда: .env и TLS в secrets/, туда, где
# их ищет рабочая копия. Ключ, known_hosts, адрес и секреты -- защищённые
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

# ssh отвергает ключ, который читают другие: копия в файле 0600.
# Перевод строки дописан: без него в конце OpenSSH ключ не читает, а
# файловая переменная GitLab его не гарантирует.
key="$work/key"
{ cat "$BROOD_DEPLOY_SSH_KEY"; echo; } >"$key"
chmod 0600 "$key"

# Секреты -- под своими именами в рабочей копии, 0600: tar на машине права
# сохранит, а umask их только сужает.
stage="$work/stage"
install -d -m 0700 "$stage/secrets"
install -m 0600 "$BROOD_DEPLOY_ENV" "$stage/.env"
install -m 0600 "$BROOD_DEPLOY_TLS_CERT" "$stage/secrets/tls.crt"
install -m 0600 "$BROOD_DEPLOY_TLS_KEY" "$stage/secrets/tls.key"

tree="$work/tree.tar"
git archive --format=tar -o "$tree" "$CI_COMMIT_SHA"
tar --append -f "$tree" -C "$stage" .env secrets

# -F /dev/null: настройки ssh пользователя раннера сюда не доезжают.
ssh -F /dev/null -i "$key" -o IdentitiesOnly=yes -o BatchMode=yes \
    -o StrictHostKeyChecking=yes -o UserKnownHostsFile="$BROOD_DEPLOY_KNOWN_HOSTS" \
    -o ServerAliveInterval=30 "$BROOD_DEPLOY_HOST" "$CI_COMMIT_SHA" <"$tree"
