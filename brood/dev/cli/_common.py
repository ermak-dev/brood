"""Общее командлетов разработчика: разбор аргументов, справка, git рабочей
копии, текст без управляющих символов."""
import argparse
import re
import subprocess
import sys


def parser(group, verb):
    """argparse глагола без встроенной справки: usage -- докстринг модуля."""
    return argparse.ArgumentParser(prog=f"brood dev {group} {verb}", add_help=False)


def usage(doc):
    """Справка и выход 1. Докстринг командлета -- единственный текст справки:
    второй экземпляр разошёлся бы с первым."""
    sys.exit(doc.strip())


def git(*args):
    """git в текущей рабочей копии. Отказ -- строкой, а не трассировкой: она
    здесь ничего не добавляет, а скрывает единственное, что нужно знать."""
    r = subprocess.run(["git", *args], capture_output=True, text=True)
    if r.returncode != 0:
        sys.exit(f"git {' '.join(args)}: {(r.stderr or r.stdout).strip()}")
    return r.stdout.strip()


def default_branch():
    """Интеграционная ветка как `origin/<имя>`. origin/HEAD выставлен не в
    каждом клоне -- откат на origin/main: отказ здесь означал бы «не могу
    завести ветку» там, где ветку завести можно."""
    r = subprocess.run(["git", "rev-parse", "--abbrev-ref", "origin/HEAD"],
                       capture_output=True, text=True)
    return r.stdout.strip() if r.returncode == 0 else "origin/main"


# Управляющие последовательности и символы строки чужого вывода: в выводе
# они красили бы его и двигали курсор.
_ESCAPES = re.compile(r"\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b[@-_]|[\x00-\x08\x0b-\x1f\x7f]")


def plain(text):
    """Текст без цветов и управляющих символов терминала: вывод командлета
    читает модель, а не терминал."""
    return _ESCAPES.sub("", text)
