#!/bin/bash
# Подключается внутри контейнера. Вызывающий владеет временным каталогом.
: "${work:?caller must create and clean a private temporary directory}"
read -r version checksum < <(python3 -I -c \
    'import json; p=json.load(open("brood/dev/pins.json")); print(p["GO_VERSION"],p["GO_SHA256"])')
mkdir "$work/toolchain"
bash "$(dirname "${BASH_SOURCE[0]}")/go-download.sh" "$version" "$checksum" "$work/go.tar.gz"
printf '%s  %s\n' "$checksum" "$work/go.tar.gz" | sha256sum --check --status
tar -xzf "$work/go.tar.gz" -C "$work/toolchain"
export PATH="$work/toolchain/go/bin:$PATH"
export CGO_ENABLED=0 GOTOOLCHAIN=local GOWORK=off GOFLAGS=-modcacherw
export GOCACHE="$work/build-cache" GOMODCACHE="$work/module-cache"
