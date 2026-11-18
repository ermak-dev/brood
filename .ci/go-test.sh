#!/bin/bash
# Go toolchain и Python-образ закреплены общими pins; окружение раннера
# не передаётся внутрь контейнера проверок.
set -o errexit -o nounset -o pipefail

if [[ "${1:-}" != "--inside" ]]; then
    bash .ci/web.sh verify
    image="$(python3 -I -c 'import json; print(json.load(open("brood/dev/pins.json"))["PYTHON_IMAGE"])')"
    exec docker run --rm --platform linux/amd64 --user "$(id -u):$(id -g)" \
        --env HOME=/tmp --volume "$PWD:/src" --workdir /src \
        --tmpfs /bt:rw,nodev,nosuid,exec,mode=1777,size=2g \
        "$image" bash .ci/go-test.sh --inside
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
source .ci/go-toolchain.sh
cd go
for attempt in 1 2 3; do
    printf 'Downloading Go modules: attempt %s/3\n' "$attempt"
    if timeout --signal=TERM --kill-after=10s 600s go mod download; then
        break
    fi
    if (( attempt == 3 )); then
        printf '%s\n' 'Go module download failed after 3 bounded attempts' >&2
        exit 1
    fi
    sleep 2
done
make fmt-check
make vet
# Use the same short, private path for Go scratch and test process temp files.
# The toolchain can use GOTMPDIR for tests even when TMPDIR is set separately.
if [[ "$(stat -f -c %T /bt)" != tmpfs ]]; then
    printf '%s\n' 'Go test scratch must be a container tmpfs' >&2
    exit 1
fi
printf '%s\n' 'Go test scratch: container tmpfs (TMPDIR and GOTMPDIR)'
TMPDIR=/bt GOTMPDIR=/bt make test
make build
make cross
for arch in amd64 arm64; do
    tar -czf "bin/brood-linux-${arch}.tar.gz" -C "bin/linux_${arch}" brood bro
done
