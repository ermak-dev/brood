#!/bin/bash
# Частичный архив принадлежит только этому вызову; ни один отказ не выдаёт его как готовый.
set -o errexit -o nounset -o pipefail
[[ $# == 3 ]] || { printf '%s\n' 'Go version, SHA-256 and output path are required' >&2; exit 2; }
version=$1 checksum=$2 output=$3
[[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+$ && $checksum =~ ^[0-9a-f]{64}$ ]] || {
    printf '%s\n' 'Invalid pinned Go version or SHA-256' >&2
    exit 2
}
[[ ! -e $output && ! -L $output ]] || { printf '%s\n' 'Go archive output already exists' >&2; exit 1; }
partial=$(mktemp "${output}.partial.XXXXXX")
trap 'rm -f -- "$partial"' EXIT
url="https://go.dev/dl/go${version}.linux-amd64.tar.gz"
for attempt in {1..8}; do
    printf 'Downloading Go %s: attempt %s/8\n' "$version" "$attempt" >&2
    if curl --fail --silent --show-error --location --connect-timeout 20 --max-time 300 \
            --speed-limit 1024 --speed-time 60 --continue-at - "$url" --output "$partial"; then
        if ! printf '%s  %s\n' "$checksum" "$partial" | sha256sum --check --status; then
            printf '%s\n' 'Go archive SHA-256 does not match the pinned checksum' >&2
            exit 1
        fi
        mv --no-clobber -- "$partial" "$output"
        [[ ! -e $partial ]] || { printf '%s\n' 'Go archive output appeared during download' >&2; exit 1; }
        exit 0
    fi
    if (( attempt < 8 )); then sleep 2; fi
done
printf '%s\n' 'Go archive download failed after 8 bounded attempts' >&2
exit 1
