#!/bin/bash
# Go toolchain и Python-образ закреплены общими pins; окружение раннера
# не передаётся внутрь контейнера проверок.
set -o errexit -o nounset -o pipefail

if [[ "${1:-}" != "--inside" ]]; then
    image="$(python3 -I -c 'import json; print(json.load(open("brood/dev/pins.json"))["PYTHON_IMAGE"])')"
    exec docker run --rm --platform linux/amd64 --user "$(id -u):$(id -g)" \
        --env HOME=/tmp --volume "$PWD:/src" --workdir /src \
        "$image" bash .ci/go-test.sh --inside
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
read -r version checksum < <(python3 -I -c \
    'import json; p=json.load(open("brood/dev/pins.json")); print(p["GO_VERSION"],p["GO_SHA256"])')
mkdir "$work/toolchain"
curl --fail --silent --show-error --location \
    "https://go.dev/dl/go${version}.linux-amd64.tar.gz" --output "$work/go.tar.gz"
printf '%s  %s\n' "$checksum" "$work/go.tar.gz" | sha256sum --check --status
tar -xzf "$work/go.tar.gz" -C "$work/toolchain"
export PATH="$work/toolchain/go/bin:$PATH"
export CGO_ENABLED=0 GOTOOLCHAIN=local GOWORK=off GOFLAGS=-modcacherw
export GOCACHE="$work/build-cache" GOMODCACHE="$work/module-cache"
cd go
go mod download
make fmt-check
make vet
make test
make build
make cross
for arch in amd64 arm64; do
    tar -czf "bin/brood-linux-${arch}.tar.gz" -C "bin/linux_${arch}" brood bro brood-migrate
done
