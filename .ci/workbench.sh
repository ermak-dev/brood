#!/bin/bash
# Полный цикл внутри development-актора. Порты и Docker относятся к его VM,
# а не к машине установки; после ошибки среда остаётся доступна для разбора.
set -o errexit -o nounset -o pipefail

: "${WORKBENCH_ROOT:?WORKBENCH_ROOT is required}"
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

status testing "Installing declared dependencies and running Python checks"
bin/brood dev setup
bin/brood server test -v 2
venv/bin/ruff check .

status testing "Checking and building the web panel"
bin/brood dev web test

status deploying "Installing an isolated Substrate cluster inside this actor"
bin/brood substrate install
bin/brood substrate start

status deploying "Building and deploying Brood into the isolated cluster"
bin/brood deploy server

status passed "Python checks, panel verification, image builds and test deployment passed"
printf 'Workbench build, tests and isolated deployment passed\n'
exec sleep infinity
