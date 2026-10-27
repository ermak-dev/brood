"""brood dev setup: create the working copy's venv and development settings

Creates venv/ in the brood working copy with the python that runs brood, or
reuses the one already there, and installs the dev dependencies declared
in pyproject.toml. bin/brood activates this venv on every call.

Adds a generated BROOD_SESSION_SECRET when .env lacks it. Existing values
are preserved and the secret is never printed. Build the Go binaries with
make -C go build; the server and client do not use this Python environment.
"""
import os
import secrets
import subprocess
import sys
import tomllib

from brood.common import config
from brood.dev.cli._common import usage

VENV = os.path.join(config.PROJECT, "venv")
PYPROJECT = os.path.join(config.PROJECT, "pyproject.toml")
EXTRAS = ("dev",)


def requirements(project, extras=EXTRAS):
    """Разобранный pyproject.toml -> зависимости названных extras. Чистая
    функция. Неизвестный extra -- отказ: пропущенный молча, он оставил бы
    venv без зависимостей, и узнали бы об этом на первом импорте."""
    declared = project.get("project", {}).get("optional-dependencies", {})
    missing = [e for e in extras if e not in declared]
    if missing:
        raise RuntimeError(f"pyproject.toml declares no extra {', '.join(missing)}")
    return [dep for e in extras for dep in declared[e]]


def missing_settings(env, new_key):
    """Что дописать в .env; существующий непустой секрет сохраняется."""
    return [] if env.get("BROOD_SESSION_SECRET") else [("BROOD_SESSION_SECRET", new_key)]


def append_lines(text, pairs):
    """Хвост, который дописать к .env с текстом text. Чистая функция: без
    перевода строки в конце файла первая новая строка склеилась бы с
    последней старой."""
    lines = "".join(f"{name}='{value}'\n" for name, value in pairs)
    return ("\n" if text and not text.endswith("\n") else "") + lines


def _run(argv, what):
    r = subprocess.run(argv, capture_output=True, text=True)
    if r.returncode != 0:
        raise RuntimeError(f"{what}: {(r.stderr or r.stdout).strip()}")


def _read(path):
    try:
        with open(path, encoding="utf-8") as f:
            return f.read()
    except FileNotFoundError:
        return ""
    except OSError as e:
        raise RuntimeError(f"cannot read {path}: {e.strerror}") from None


def main(argv):
    if argv:
        usage(__doc__)
    with open(PYPROJECT, "rb") as f:
        deps = requirements(tomllib.load(f))
    python = os.path.join(VENV, "bin", "python")
    if not os.path.exists(python):
        _run([sys.executable, "-m", "venv", VENV], "cannot create the venv")
    _run([python, "-m", "pip", "install", "--quiet", *deps], "pip install failed")
    print(f"venv {VENV}: {', '.join(deps)}")

    text = _read(config.ENV_FILE)
    pairs = missing_settings(config.parse_env(text), secrets.token_urlsafe(50))
    if pairs:
        # 0600 у нового файла: в .env секреты.
        fd = os.open(config.ENV_FILE, os.O_WRONLY | os.O_APPEND | os.O_CREAT, 0o600)
        with os.fdopen(fd, "a", encoding="utf-8") as f:
            f.write(append_lines(text, pairs))
        print(f".env {config.ENV_FILE}: added {', '.join(name for name, _ in pairs)}")
    print("next: make -C go build; go/bin/brood bootstrap-admin; go/bin/brood serve")
