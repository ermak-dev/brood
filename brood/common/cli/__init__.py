"""Диспетчер командлетов: `brood <группа> [<глагол> ...] [аргументы]`.

Порт диспетчера mop. Командлеты модуля живут в его пакете cli; группа --
подпакет, глагол -- модуль в нём:
  brood/dev/cli/__init__.py      ->  brood dev
  brood/dev/cli/bug/__init__.py  ->  brood dev bug
  brood/dev/cli/bug/list.py      ->  brood dev bug list
Пакет без названного глагола отвечает сам и получает остаток argv.

Группы находятся обходом каталогов, а не списком и не импортом по имени:
дистрибутив без модуля не видит его команд, а common не зависит от
остальных модулей.

Контракт команды: модуль с докстрингом (первая строка -- описание в help,
весь текст -- usage) и `main(argv)`, которая возвращает код выхода или None.
Печатают только пакеты cli; остальной код возвращает данные и молчит.
Описание для help читается разбором AST, без импорта модуля.
"""
import ast
import importlib
import os
import sys

PACKAGE = os.path.dirname(os.path.dirname(os.path.dirname(os.path.realpath(__file__))))
# Пакет cli модуля common -- сам диспетчер, а не группа команд.
SELF = "common"


def groups(package=PACKAGE):
    """Обход модулей -> {группа: дерево глаголов}. Группа -- модуль brood с
    пакетом cli. В дереве модуль -- {}, подпакет -- его дерево; служебное
    (_*) -- не глаголы."""
    out = {}
    for entry in sorted(os.listdir(package)):
        cli = os.path.join(package, entry, "cli")
        if entry != SELF and not entry.startswith("_") and _is_package(cli):
            out[entry] = _tree(cli)
    return out


def _is_package(path):
    return os.path.isfile(os.path.join(path, "__init__.py"))


def _tree(path):
    out = {}
    for entry in sorted(os.listdir(path)):
        full = os.path.join(path, entry)
        if entry.startswith("_"):
            continue
        if entry.endswith(".py"):
            out[entry[:-3]] = {}
        elif _is_package(full):
            out[entry] = _tree(full)
    return out


def resolve(argv, tree):
    """argv -> (модуль, остаток argv) либо None, если группы нет. Чистая
    функция. Спуск по дереву: подпакет -- ещё уровень, модуль -- дно."""
    if not argv or argv[0] not in tree:
        return None
    mod, tree, rest = f"brood.{argv[0]}.cli", tree[argv[0]], list(argv[1:])
    while rest and rest[0] in tree:
        mod, tree, rest = f"{mod}.{rest[0]}", tree[rest[0]], rest[1:]
    return mod, rest


def docstring(path):
    """Докстринг модуля целиком, без импорта; пусто, если его нет."""
    try:
        with open(path, encoding="utf-8") as f:
            doc = ast.get_docstring(ast.parse(f.read()))
    except (OSError, SyntaxError):
        return ""
    return (doc or "").strip()


def describe(path):
    """Первая строка докстринга модуля; пусто, если его нет."""
    doc = docstring(path)
    return doc.splitlines()[0].strip() if doc else ""


def help_text(tree, package=PACKAGE):
    """Список групп -- ответ на `brood` без аргументов."""
    lines = ["brood: an agent pool on Agent Substrate", "",
             "  brood <group> <verb> [arguments]; a group alone lists its verbs", ""]
    lines += [f"  {g:<10} {describe(os.path.join(package, g, 'cli', '__init__.py'))}"
              for g in tree]
    lines += ["", "  brood help  this list"]
    return "\n".join(lines)


def main(argv):
    tree = groups()
    if not argv or argv[0] in ("help", "-h", "--help"):
        print(help_text(tree))
        return 0
    found = resolve(argv, tree)
    if found is None:
        print(f"brood: unknown command {argv[0]!r}; run 'brood help'", file=sys.stderr)
        return 1
    modname, rest = found
    return run(lambda a: importlib.import_module(modname).main(a), rest)


def run(fn, argv):
    """Команда, чьи ожидаемые отказы -- одна строка stderr и код 1, без
    трассировки: вывод читает модель, а трасса прячет причину.
    RuntimeError -- отказ библиотеки, OSError -- сети и файлов, ImportError
    -- модуля, которого нет в этом дистрибутиве."""
    try:
        return fn(argv) or 0
    except (RuntimeError, OSError, ImportError) as e:
        print(e, file=sys.stderr, flush=True)
        return 1
    except KeyboardInterrupt:
        return 130


def console():
    """Вход пакета: console_scripts и `python -m brood.common.cli`."""
    sys.exit(main(sys.argv[1:]))
