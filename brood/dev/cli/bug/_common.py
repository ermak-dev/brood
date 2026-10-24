"""Общее глаголам трекера: тело задачи и разбор аргументов."""
import functools
import sys

from brood.dev.cli import _common as dev_common


def read_body(text, path):
    """Тело задачи: --body-file, аргумент или stdin. Через файл и stdin -- ради
    бэктиков и переносов: шелл их съедает, а тело задачи без них не написать."""
    if path == "-":
        return sys.stdin.read().strip()
    if path:
        # encoding явно: тела задач по-русски, а кодировка по умолчанию
        # берётся из локали процесса, то есть зависит от машины.
        try:
            with open(path, encoding="utf-8") as f:
                return f.read().strip()
        except OSError as e:
            sys.exit(f"--body-file {path}: {e.strerror or e}")
    if text and text != "-":
        return text.strip()
    return sys.stdin.read().strip()


parser = functools.partial(dev_common.parser, "bug")
