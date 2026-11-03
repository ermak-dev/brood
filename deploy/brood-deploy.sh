#!/bin/bash
# Принудительная команда ключа деплоя (docs/DEPLOY.md). authorized_keys
# пользователя brood зовёт её вместо любой команды, присланной клиентом, и
# больше ключ не умеет ничего.
#
# Клиент -- джоба deploy на builder (.ci/deploy.sh) -- шлёт полный sha коммита
# командой ssh (SSH_ORIGINAL_COMMAND), а на stdin -- tar его дерева (git
# archive) с дописанными .env и secrets/ стенда. Дерево распаковывается в
# чистый releases/<sha>. Обновляется только приложение: работающий Substrate
# и его акторы не перекатываются вместе с сервером. Состояние установки
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
export BROOD_RELEASE_SHA="$SHA"
bin/brood deploy server --check
bin/brood deploy server

# Артефакты бутстрапа песочницы (#156): собираем brood-guest и bro из
# релизного дерева, качаем claude по пинам и публикуем в PVC сервера
# (образ сервера scratch — поток разбирает сам бинарь через kubectl exec -i).
# Сервер перечитывает манифест на каждый запрос: рестарт не нужен.
artifacts="$(mktemp -d)"
artifacts_kubectl() {
    kubectl --context=kind-substrate -n brood "$@"
}
read -r claude_version claude_sha256 < <(python3 -I -c \
    'import json, sys; p=json.load(open(sys.argv[1])); print(p["CLAUDE_VERSION"], p["CLAUDE_SHA256"])' \
    brood/dev/pins.json)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags="-s -w" \
    -o "$artifacts/brood-guest" ./go/cmd/brood-guest \
    && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags="-s -w" \
    -o "$artifacts/bro" ./go/cmd/brood \
    || { echo "brood-deploy: guest artifact build failed" >&2; exit 1; }
curl -fsSL -o "$artifacts/claude" \
    "https://downloads.claude.ai/claude-code-releases/${claude_version}/linux-x64/claude" \
    && printf '%s  %s\n' "$claude_sha256" "$artifacts/claude" | sha256sum --check --status \
    || { echo "brood-deploy: claude artifact download failed" >&2; exit 1; }
{ printf 'bro\0'; cat "$artifacts/bro"; printf '\0brood-guest\0'; cat "$artifacts/brood-guest";
  printf '\0claude\0'; cat "$artifacts/claude"; printf '\0'; } \
    | artifacts_kubectl exec -i deployment/brood-go -- /brood artifacts --dir /data/artifacts \
    || { echo "brood-deploy: guest artifact publish failed" >&2; exit 1; }
rm -rf "$artifacts"

ln -sfn "$RELEASE" "$RELEASES/current"
find "$RELEASES" -mindepth 1 -maxdepth 1 -type d ! -name "$SHA" -exec rm -rf {} +
