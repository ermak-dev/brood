"""Общий entrypoint контейнерной сборки исходной панели."""
from pathlib import Path
import subprocess

from brood.common import config


def run(script):
    """Запустить общий CI-скрипт; Bun и версия панели закреплены в сборке."""
    if script not in {"build", "verify"}:
        raise ValueError("unsupported web script")
    root = Path(config.PROJECT)
    try:
        return subprocess.run(["bash", str(root / ".ci/web.sh"), script], cwd=root).returncode
    except FileNotFoundError:
        raise RuntimeError("bash is not installed") from None
