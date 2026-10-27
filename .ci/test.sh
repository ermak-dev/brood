#!/bin/bash
# Проверки brood в закреплённом образе python, теми же командами, что у
# разработчика: brood dev setup ставит venv из extras pyproject.toml,
# brood server test гоняет все проверки, ruff -- линтер из того же venv.
#
# Раннер -- shell-исполнитель builder (тег docker), как у mop: image: он не
# видит, и шаги шли бы на самом хосте, где нет python3-venv. Поэтому задача --
# один docker run. Контейнер -- от пользователя раннера: venv/ и .env,
# которые он пишет в checkout, следующая джоба вычистит сама. Окружения
# раннера и секретов установки в контейнере нет; DJANGO_SECRET_KEY заводит
# brood dev setup. Образ полный, не slim: проверкам brood substrate нужен
# настоящий git, а ставить пакеты непривилегированный контейнер не может.
set -o errexit -o nounset -o pipefail

IMAGE="$(python3 -I -c 'import json, sys; print(json.load(open(sys.argv[1]))["PYTHON_IMAGE"])' brood/dev/pins.json)"

exec docker run --rm --user "$(id -u):$(id -g)" --env HOME=/tmp \
    --volume "$PWD:/src" --workdir /src "$IMAGE" \
    bash -c 'bin/brood dev setup && bin/brood server test -v 2 && venv/bin/ruff check .'
