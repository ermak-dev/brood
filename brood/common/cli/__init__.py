"""Диспетчер командлетов: `brood <группа> [<глагол> ...] [аргументы]`.

Порт диспетчера mop. Группа -- пакет cli модуля или подпакета модуля,
глагол -- модуль в нём:
  brood/dev/cli/__init__.py            ->  brood dev
  brood/dev/cli/bug/list.py            ->  brood dev bug list
  brood/dev/substrate/cli/__init__.py  ->  brood substrate
  brood/dev/substrate/cli/start.py     ->  brood substrate start
Пакет без названного глагола отвечает сам и получает остаток argv.

Подпакет со своим cli -- пространство команд, которое не названо модулем:
код остаётся в модуле своего дистрибутива, а команда -- короткой.

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


def scan(package=PACKAGE):
    """Обход модулей -> {группа: модуль её пакета cli}. Группа -- модуль brood
    с пакетом cli либо подпакет модуля с пакетом cli. Одно имя в двух местах
    -- отказ, а не «кто первый»: разойдясь, две одноимённые группы молча
    выбирались бы порядком обхода."""
    top = os.path.basename(package)
    found = []
    for module in _entries(package):
        if module == SELF:
            continue
        if _is_package(os.path.join(package, module, "cli")):
            found.append((module, f"{top}.{module}.cli"))
        for sub in _entries(os.path.join(package, module)):
            if sub != "cli" and _is_package(os.path.join(package, module, sub, "cli")):
                found.append((sub, f"{top}.{module}.{sub}.cli"))
    out = {}
    for name, mod in found:
        if name in out:
            raise RuntimeError(f"command group {name} is defined twice: {out[name]} and {mod}")
        out[name] = mod
    return out


def _entries(path):
    """Подкаталоги-пакеты без служебных (_*)."""
    return [e for e in sorted(os.listdir(path))
            if not e.startswith("_") and _is_package(os.path.join(path, e))]


def location(mod, package=PACKAGE):
    """Модуль пакета cli -> его каталог."""
    return os.path.join(os.path.dirname(package), *mod.split("."))


def groups(package=PACKAGE, modules=None):
    """{группа: дерево глаголов}; modules -- готовый scan(). В дереве модуль --
    {}, подпакет -- его дерево; служебное (_*) -- не глаголы."""
    modules = scan(package) if modules is None else modules
    return {name: _tree(location(mod, package)) for name, mod in modules.items()}


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


def resolve(argv, tree, modules=None):
    """argv -> (модуль, остаток argv) либо None, если группы нет. tree --
    groups(), modules -- scan(); с обоими это чистая функция. Спуск по
    дереву: подпакет -- ещё уровень, модуль -- дно."""
    if not argv or argv[0] not in tree:
        return None
    modules = scan() if modules is None else modules
    mod, tree, rest = modules[argv[0]], tree[argv[0]], list(argv[1:])
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


def help_text(modules, package=PACKAGE):
    """Список групп -- ответ на `brood` без аргументов."""
    lines = ["brood: an agent pool on Agent Substrate", "",
             "  brood <group> <verb> [arguments]; a group alone lists its verbs", ""]
    lines += [f"  {g:<10} {describe(os.path.join(location(m, package), '__init__.py'))}"
              for g, m in sorted(modules.items())]
    lines += ["", "  brood help  this list"]
    return "\n".join(lines)


def main(argv):
    try:
        modules = scan()
    except RuntimeError as e:
        print(f"brood: {e}", file=sys.stderr)
        return 1
    if not argv or argv[0] in ("help", "-h", "--help"):
        print(help_text(modules))
        return 0
    def dispatch(args):
        found = resolve(args, groups(modules=modules), modules)
        if found is None:
            print(f"brood: unknown command {args[0]!r}; run 'brood help'", file=sys.stderr)
            return 1
        modname, rest = found
        return importlib.import_module(modname).main(rest)

    def invoke(args):
        if args[0] in modules:
            package = importlib.import_module(modules[args[0]])
            wrapper = getattr(package, "dispatch", None)
            if wrapper:
                return wrapper(args, dispatch)
        return dispatch(args)

    return run(invoke, argv)


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
