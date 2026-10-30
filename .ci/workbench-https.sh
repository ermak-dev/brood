#!/bin/bash
set -o errexit -o nounset -o pipefail

: "${WORKBENCH_ROOT:?WORKBENCH_ROOT is required}"
: "${SUBSTRATE_SOURCE:?SUBSTRATE_SOURCE is required}"
if [[ "${BROOD_WORKBENCH:-}" != 1 || "${DOCKER_HOST:-}" != unix:///var/run/docker.sock ||
      "$SUBSTRATE_SOURCE" != "$WORKBENCH_ROOT/substrate" ]]; then
    printf '%s\n' 'Private HTTPS probe requires the isolated workbench guest' >&2
    exit 1
fi
kubectl="$SUBSTRATE_SOURCE/bin/substrate/kubectl"
export KUBECONFIG="$SUBSTRATE_SOURCE/bin/substrate/kubeconfig"
ca="$WORKBENCH_ROOT/brood/secrets/tls.crt"
[[ -x "$kubectl" && -r "$KUBECONFIG" && -r "$ca" ]] || {
    printf '%s\n' 'Private HTTPS probe requires the guest kubeconfig, kubectl and CA' >&2
    exit 1
}
log=$(mktemp "$WORKBENCH_ROOT/logs/go-https-forward.XXXXXX.log")
body=$(mktemp "$WORKBENCH_ROOT/logs/go-https-response.XXXXXX")
"$kubectl" --context=kind-substrate -n brood port-forward --address=127.0.0.1 \
    service/brood-go :8443 > "$log" 2>&1 &
forward=$!
trap 'kill "$forward" 2>/dev/null || true; wait "$forward" 2>/dev/null || true; rm -f -- "$body"' EXIT
port=
ready='^Forwarding from 127[.]0[.]0[.]1:([0-9]{1,5}) -> 8443$'
for ((attempt = 0; attempt < 30; attempt++)); do
    kill -0 "$forward" 2>/dev/null || break
    while IFS= read -r line; do
        if [[ "$line" =~ $ready ]]; then
            port=$((10#${BASH_REMATCH[1]}))
            break
        fi
    done < "$log"
    [[ -n "$port" ]] && break
    sleep 1
done
if [[ -z "$port" ]] || (( port < 1 || port > 65535 )); then
    printf 'Private Go port-forward is not ready; log: %s\n' "$log" >&2
    exit 1
fi
for ((attempt = 0; attempt < 30; attempt++)); do
    kill -0 "$forward" 2>/dev/null || break
    if code=$(curl --disable --fail --silent --show-error --max-time 2 --noproxy '*' \
            --cacert "$ca" --output "$body" --write-out '%{http_code}' \
            "https://127.0.0.1:$port/healthz") && [[ "$code" == 200 && "$(< "$body")" == ok ]] &&
            kill -0 "$forward" 2>/dev/null; then
        printf '%s\n' 'Private Go HTTPS check passed'
        exit 0
    fi
    sleep 1
done
printf 'Private Go HTTPS check failed; port-forward log: %s\n' "$log" >&2
exit 1
