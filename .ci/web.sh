#!/bin/bash
# Общая сборка панели: один Bun pin из Dockerfile, без секретов установки.
set -o errexit -o nounset -o pipefail
root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
script="${1:-verify}"
case "$script" in build|verify|test) ;; *) printf 'unsupported web script: %s\n' "$script" >&2; exit 2 ;; esac

if [[ "${2:-}" != "--inside" ]]; then
    image="$(grep -E '^FROM .* AS panel$' "$root/deploy/Dockerfile.go" | cut -d ' ' -f 2)"
    [[ -n "$image" ]] || { printf 'Dockerfile.go has no pinned panel stage\n' >&2; exit 1; }
    exec docker run --rm --platform linux/amd64 --user "$(id -u):$(id -g)" \
        --env HOME=/tmp --volume "$root/web:/src/web" \
        --volume "$root/go/brood/server/static:/src/go/brood/server/static" \
        --volume "$root/.ci/web.sh:/src/.ci/web.sh:ro" \
        --volume "$root/deploy/Dockerfile.go:/src/deploy/Dockerfile.go:ro" \
        --workdir /src "$image" bash .ci/web.sh "$script" --inside
fi

cd "$root/web"
bun install --frozen-lockfile
VERSION=v1.26.7+brood bun run "$script"
if [[ "$script" != test ]]; then
    # Только успешная сборка заменяет tracked release bundle. Прямая Go-сборка
    # использует его без Bun; секретов backend в frontend environment нет.
    cp dist/index.html ../go/brood/server/static/index.html.tmp
    mv ../go/brood/server/static/index.html.tmp ../go/brood/server/static/index.html
fi
