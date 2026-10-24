"""Общее глаголам `brood dev web`: Bun панели в контейнере образа входа."""
import os
import re
import subprocess

from brood.common import config

WEB = os.path.join(config.PROJECT, "brood", "server", "web")
EDGE_DOCKERFILE = os.path.join(config.PROJECT, "brood", "dev", "deploy", "edge", "Dockerfile")
_STAGE = re.compile(r"^FROM\s+(\S+)\s+AS\s+panel\s*$", re.M | re.I)


def bun_image(dockerfile_text):
    """Текст Dockerfile входа -> образ Bun его стадии panel. Чистая функция:
    версия Bun живёт в одном месте, и сборка разработчика не расходится с
    сборкой выкладки."""
    m = _STAGE.search(dockerfile_text)
    if not m:
        raise RuntimeError(f"{EDGE_DOCKERFILE} has no 'FROM <image> AS panel' stage")
    return m.group(1)


def run(script):
    """bun install и bun run <script> в каталоге панели -> код выхода. Вывод
    -- в терминал: его печатает Bun."""
    with open(EDGE_DOCKERFILE, encoding="utf-8") as f:
        image = bun_image(f.read())
    argv = ["docker", "run", "--rm", "--user", f"{os.getuid()}:{os.getgid()}",
            "-e", "HOME=/tmp", "-v", f"{WEB}:/app", "-w", "/app", image,
            "sh", "-ec", f"bun install --frozen-lockfile && bun run {script}"]
    try:
        return subprocess.run(argv).returncode
    except FileNotFoundError:
        raise RuntimeError("docker is not installed") from None
