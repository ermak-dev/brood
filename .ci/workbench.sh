#!/bin/bash
# Полный цикл внутри development-актора. Docker и kind принадлежат его VM.
set -o errexit -o nounset -o pipefail

: "${WORKBENCH_ROOT:?WORKBENCH_ROOT is required}"
: "${BROOD_WORKBENCH:?run inside the isolated workbench guest}"
[[ "$BROOD_WORKBENCH" == 1 && "$DOCKER_HOST" == unix:///var/run/docker.sock ]]
cd "${WORKBENCH_ROOT}/brood"
exec > >(tee -a "${WORKBENCH_ROOT}/results/workbench.log") 2>&1

status() {
    printf '{"state":"%s","message":"%s"}\n' "$1" "$2" \
        > "${WORKBENCH_ROOT}/results/status.tmp"
    mv "${WORKBENCH_ROOT}/results/status.tmp" "${WORKBENCH_ROOT}/results/status.json"
}
failed() {
    code=$?
    status failed "A build, test or deployment step failed; see workbench.log"
    printf 'Workbench job failed with exit code %s\n' "$code"
    exec sleep infinity
}
trap failed ERR

status testing "Installing declared dependencies and running Python dev checks"
bin/brood dev setup
bin/brood dev test -v
venv/bin/ruff check .

status testing "Testing the restored React panel and rebuilding the embedded bundle"
bin/brood dev web test

status testing "Testing Go server and client; building Go binaries"
export GOTOOLCHAIN=local CGO_ENABLED=0
(
    cd go
    go test ./...
    go build -trimpath -o bin/ ./cmd/brood ./cmd/bro
)
export PATH="$PWD/go/bin:$PATH"

status deploying "Installing an isolated Substrate cluster inside this actor"
bin/brood substrate install
bin/brood substrate start

status deploying "Preparing guest infrastructure and deploying the private Go endpoint"
bin/brood workbench prepare
bin/brood deploy go

status passed "Python dev checks, Go tests, binary builds and isolated Go endpoint deployment passed"
printf 'Workbench build, tests and isolated deployment passed\n'
exec sleep infinity
