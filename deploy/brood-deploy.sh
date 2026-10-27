#!/bin/bash
# Принудительная команда ключа деплоя (docs/DEPLOY.md). authorized_keys
# пользователя brood зовёт её вместо любой команды, присланной клиентом, и
# больше ключ не умеет ничего.
#
# Клиент -- джоба deploy на builder (.ci/deploy.sh) -- шлёт полный sha коммита
# командой ssh (SSH_ORIGINAL_COMMAND), а на stdin -- tar его дерева (git
# archive) с дописанными .env и secrets/ стенда. Дерево распаковывается в
# чистый releases/<sha>, и из него ставится Substrate -- brood substrate
# install, затем start, -- и сервер: brood deploy server. Состояние установки
# живёт в каталоге данных пользователя, не в релизе: старый релиз вместе с его
# секретами удаляется после удачного деплоя.
set -o errexit -o nounset -o pipefail

SHA="${SSH_ORIGINAL_COMMAND:-}"
if [[ ! "$SHA" =~ ^[0-9a-f]{40}$ ]]; then
    echo "brood-deploy: expected a full commit sha, got '${SHA}'" >&2
    exit 2
fi

RELEASES="$HOME/releases"
mkdir -p "$RELEASES"
# Два деплоя сразу дрались бы за один кластер: второй -- отказ, не ожидание.
exec 9>"$RELEASES/.lock"
if ! flock -n 9; then
    echo "brood-deploy: another deploy is running" >&2
    exit 1
fi

RELEASE="$RELEASES/$SHA"
rm -rf "$RELEASE"
mkdir "$RELEASE"
tar -x -C "$RELEASE" --no-same-owner
cd "$RELEASE"
echo "brood-deploy: ${SHA}"
# Проверка публичного cutover должна отказать до первой мутации кластера.
# Подготовка приватного Go endpoint не означает готовности пользователей.
bin/brood deploy server --check
bin/brood substrate install
bin/brood substrate start
bin/brood deploy server

ln -sfn "$RELEASE" "$RELEASES/current"
find "$RELEASES" -mindepth 1 -maxdepth 1 -type d ! -name "$SHA" -exec rm -rf {} +
