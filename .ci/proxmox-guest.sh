#!/bin/bash
# Контекст берётся из HEAD: локальные настройки, ключи и ignored-файлы не читаются.
set -o errexit -o nounset -o pipefail
fail() { printf '%s\n' "$*" >&2; exit 1; }
[[ $# == 1 || ( $# == 2 && $2 == --seed ) ]] || {
    printf 'Usage: %s OUTPUT_DIRECTORY [--seed]\n' "$0" >&2
    exit 2
}
output=$1
[[ -n $output ]] || fail 'Output directory is required'
root=$(git -C "$(dirname -- "${BASH_SOURCE[0]}")/.." rev-parse --show-toplevel)
git -C "$root" diff --quiet HEAD -- || fail 'Source has tracked changes; commit before building'
commit=$(git -C "$root" rev-parse --verify HEAD)
[[ $commit =~ ^[0-9a-f]{40}$ ]] || fail 'Invalid source commit'
# Существующий пустой каталог допустим; непустой или symlink никогда не трогаем.
[[ ! -L $output ]] || fail 'Output directory must not be a symlink'
if [[ -e $output ]]; then
    [[ -d $output && -z $(find "$output" -mindepth 1 -maxdepth 1 -print -quit) ]] || fail 'Output directory is not empty'
else
    mkdir -- "$output"
fi
output=$(realpath -- "$output")
[[ $output != *,* ]] || fail 'Output path must not contain a comma'
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
mkdir "$work/source" "$work/context" "$work/bin" "$work/context/seed"
# Явный список файлов компилятора отделён от финального контекста образа.
git -C "$root" archive "$commit" go .ci/go-download.sh brood/dev/pins.json \
    deploy/Dockerfile.proxmox-guest deploy/proxmox-guest-dhcp.sh \
    deploy/driftty-storage.js deploy/driftty-storage.test.cjs | tar -x -C "$work/source"
[[ -f $work/source/go/cmd/brood-guest/main.go ]] || fail 'Committed cmd/brood-guest is required; image build is pending integration'
cp "$work/source/deploy/Dockerfile.proxmox-guest" "$work/context/Dockerfile"
cp "$work/source/deploy/proxmox-guest-dhcp.sh" "$work/context/proxmox-guest-dhcp.sh"
cp "$work/source/deploy/driftty-storage.js" "$work/source/deploy/driftty-storage.test.cjs" "$work/context/"
cp "$work/source/.ci/go-download.sh" "$work/context/go-download.sh"
cp "$work/source/brood/dev/pins.json" "$work/context/pins.json"
# pins.json имеет каноническую однострочную запись каждого строкового ключа.
python_image=$(sed -nE 's/^  "PYTHON_IMAGE": "([^"]+)",?$/\1/p' "$work/context/pins.json")
[[ $python_image =~ ^python:3\.12@sha256:[0-9a-f]{64}$ ]] || fail 'Invalid pinned Python image'
driftty_args=()
for pin in DRIFTTY_COMMIT DRIFTTY_SHA256 DRIFTTY_INDEX_SHA256 DRIFTTY_NODE_IMAGE; do
    value=$(sed -nE "s/^  \"$pin\": \"([^\"]+)\",?$/\\1/p" "$work/context/pins.json")
    case $pin in
        DRIFTTY_COMMIT) [[ $value =~ ^[0-9a-f]{40}$ ]] ;;
        DRIFTTY_SHA256|DRIFTTY_INDEX_SHA256) [[ $value =~ ^[0-9a-f]{64}$ ]] ;;
        DRIFTTY_NODE_IMAGE) [[ $value =~ ^node:22\.12\.0-bookworm-slim@sha256:[0-9a-f]{64}$ ]] ;;
    esac || fail "Invalid pinned driftty setting: $pin"
    driftty_args+=(--build-arg "$pin=$value")
done
command -v docker >/dev/null || fail 'Docker with buildx is required'
# Компиляция тоже идёт через выбранный buildx builder, без запуска контейнера на хосте.
cat > "$work/source/Dockerfile" <<'DOCKERFILE'
ARG PYTHON_IMAGE
FROM ${PYTHON_IMAGE} AS compile
COPY brood/dev/pins.json /tmp/pins.json
COPY .ci/go-download.sh /tmp/go-download.sh
RUN python3 -I -c 'import json; p=json.load(open("/tmp/pins.json")); print(p["GO_VERSION"]); print(p["GO_SHA256"])' > /tmp/go-pins \
    && version="$(sed -n '1p' /tmp/go-pins)" \
    && checksum="$(sed -n '2p' /tmp/go-pins)" \
    && bash /tmp/go-download.sh "$version" "$checksum" /tmp/go.tar.gz \
    && tar -xzf /tmp/go.tar.gz -C /usr/local
ENV PATH=/usr/local/go/bin:${PATH} CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOTOOLCHAIN=local GOWORK=off
WORKDIR /src
COPY go/ ./
RUN go build -trimpath -buildvcs=false -ldflags="-s -w" -o /out/brood-guest ./cmd/brood-guest \
    && go build -trimpath -buildvcs=false -ldflags="-s -w" -o /out/bro ./cmd/bro
FROM scratch
COPY --from=compile /out/brood-guest /brood-guest
COPY --from=compile /out/bro /bro
DOCKERFILE
docker buildx build --platform linux/amd64 --build-arg "PYTHON_IMAGE=$python_image" \
    --output "type=local,dest=$work/bin" "$work/source"
cp "$work/bin/brood-guest" "$work/context/brood-guest"
cp "$work/bin/bro" "$work/context/bro"
if [[ $# == 2 ]]; then
    # Bundle содержит только снимок публичных tracked-файлов, без истории/remote/hooks.
    mkdir "$work/seed" "$work/git-home" "$work/git-template"
    git -C "$root" archive "$commit" | tar -x -C "$work/seed" \
        --exclude=.env --exclude='.env.*' --exclude=secrets --exclude=venv --exclude=.brood \
        --exclude=.git --exclude=.gitconfig --exclude=.git-credentials
    (
        export HOME="$work/git-home" XDG_CONFIG_HOME="$work/git-home" GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null
        unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_CONFIG_PARAMETERS GIT_CONFIG_COUNT
        git -C "$work/seed" init --quiet --template="$work/git-template" --initial-branch=main
        git -C "$work/seed" add --all
        GIT_AUTHOR_NAME=brood GIT_AUTHOR_EMAIL=source@brood.invalid \
        GIT_COMMITTER_NAME=brood GIT_COMMITTER_EMAIL=source@brood.invalid \
        GIT_AUTHOR_DATE=2000-01-01T00:00:00Z GIT_COMMITTER_DATE=2000-01-01T00:00:00Z \
            git -C "$work/seed" -c commit.gpgsign=false commit --quiet -m "Source snapshot $commit"
        git -C "$work/seed" bundle create "$work/context/seed/project.bundle" main HEAD
    )
fi
# BUILDX_BUILDER и Docker configuration остаются выбором оператора/CI.
docker buildx build --platform linux/amd64 \
    --build-arg "PYTHON_IMAGE=$python_image" --build-arg "SOURCE_COMMIT=$commit" \
    "${driftty_args[@]}" \
    --output "type=oci,dest=$work/proxmox-guest.tar" "$work/context"
[[ -s $work/proxmox-guest.tar ]] || fail 'Build produced no OCI archive'
checksum=$(sha256sum "$work/proxmox-guest.tar")
checksum=${checksum%% *}
printf '{"source_commit":"%s","archive_sha256":"%s","platform":"linux/amd64","seed":%s}\n' \
    "$commit" "$checksum" "$([[ $# == 2 ]] && printf true || printf false)" > "$work/metadata.json"
# Только законченные файлы; no-clobber защищает также от появившегося файла.
for name in proxmox-guest.tar metadata.json; do
    [[ ! -e $output/$name && ! -L $output/$name ]] || fail 'Output appeared during build; refusing overwrite'
    cp --no-clobber -- "$work/$name" "$output/$name"
    cmp --silent "$work/$name" "$output/$name" || fail 'Output changed during publication'
done
printf 'OCI archive: %s/proxmox-guest.tar\nSource commit: %s\nSHA-256: %s\n' "$output" "$commit" "$checksum"
