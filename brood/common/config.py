"""Настройки установки: окружение поверх .env рабочей копии brood.

Секреты установки живут в .env рядом с кодом; в git он не попадает.
Окружение старше файла: разовое `GITLAB_PROJECT=... brood ...` работает
без правки .env.
"""
import functools
import os

PROJECT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.realpath(__file__))))
ENV_FILE = os.path.join(PROJECT, ".env")

QUOTES = ("'", '"')


class ReadError(RuntimeError):
    """Файл настроек есть, но не читается."""


def parse_env(text):
    """Текст .env -> dict. Чистая функция.

    KEY=VALUE построчно: пустые строки и `#`-комментарии пропускаются, ключ
    отделяется первым `=`, и значение может его содержать. Парные кавычки с
    краёв значения снимаются: .env mop пишет `GITLAB_SITE='git.ermak.dev'`,
    и кавычка там часть формата, а не значения. Повтор ключа -- последний."""
    out = {}
    for line in text.splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        key, sep, value = line.partition("=")
        if not sep:
            continue
        value = value.strip()
        if len(value) >= 2 and value[0] == value[-1] and value[0] in QUOTES:
            value = value[1:-1]
        out[key.strip()] = value
    return out


def read_env(path):
    """Файл .env -> dict. Нет файла -- {}: на машине без .env работает всё,
    что секретов не касается. Иной отказ чтения -- ReadError с путём:
    проглоченный, он превратил бы недоступный файл в «настроек нет»."""
    try:
        with open(path, encoding="utf-8") as f:
            return parse_env(f.read())
    except FileNotFoundError:
        return {}
    except OSError as e:
        raise ReadError(f"cannot read settings from {path}: {e.strerror}") from e


@functools.cache
def _settings(path):
    return read_env(path)


def get(name, path=ENV_FILE):
    """Значение настройки: окружение, затем .env. Нет нигде -- пустая строка."""
    return os.environ.get(name) or _settings(path).get(name, "")
