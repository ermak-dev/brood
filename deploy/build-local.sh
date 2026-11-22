#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."
[[ $(uname -sm) == 'Linux x86_64' ]] || { printf '%s\n' 'Build on a Linux amd64 host.' >&2; exit 1; }
export GOOS=linux GOARCH=amd64 CGO_ENABLED=0
make -C go build
(cd go && go build -trimpath -ldflags='-s -w' -o bin/brood-guest ./cmd/brood-guest)
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
read -r version digest < <(python3 -I -c 'import json,sys; p=json.load(sys.stdin); print(p["CLAUDE_VERSION"],p["CLAUDE_SHA256"])' < brood/dev/pins.json)
curl --fail --location --proto '=https' --proto-redir '=https' --retry 3 -o "$work/claude" "https://downloads.claude.ai/claude-code-releases/$version/linux-x64/claude"
printf '%s  %s\n' "$digest" "$work/claude" | sha256sum --check --status || { printf '%s\n' 'Claude Code checksum mismatch; no agent artifacts published. Retry the build.' >&2; exit 1; }
for path in go/bin/bro go/bin/brood-guest "$work/claude"; do
    printf '%s\0%s\0' "${path##*/}" "$(stat -c %s "$path")"
    cat "$path" || exit 1
done | go/bin/brood artifacts --dir "$PWD/go/bin/artifacts" >/dev/null
printf '%s\n' 'Server, client and agent artifacts are ready in go/bin/.'
