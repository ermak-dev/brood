#!/bin/bash
# Проверки brood в закреплённом образе python, теми же командами, что у
# разработчика: brood dev setup ставит venv из extras pyproject.toml,
# brood dev test гоняет unittest, ruff -- линтер из того же venv.
#
# Раннер -- shell-исполнитель builder (тег docker), как у mop: image: он не
# видит, и шаги шли бы на самом хосте, где нет python3-venv. Поэтому задача --
# один docker run. Контейнер -- от пользователя раннера: venv/ и .env,
# которые он пишет в checkout, следующая джоба вычистит сама. Окружения
# раннера и секретов установки в контейнере нет.
# Образ полный, не slim: проверкам brood substrate нужен
# настоящий git, а ставить пакеты непривилегированный контейнер не может.
set -o errexit -o nounset -o pipefail

IMAGE="$(python3 -I -c 'import json, sys; print(json.load(open(sys.argv[1]))["PYTHON_IMAGE"])' brood/dev/pins.json)"

exec docker run --rm --platform linux/amd64 --user "$(id -u):$(id -g)" --env HOME=/tmp \
    --volume "$PWD:/src" --workdir /src "$IMAGE" \
    bash -euo pipefail -c 'work="$(mktemp -d)"; trap '\''rm -rf "$work"'\'' EXIT
        source .ci/go-toolchain.sh
        bin/brood dev setup
        bin/brood dev test -v
        venv/bin/ruff check .'
