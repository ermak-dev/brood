"""Раскладка кода по модулям дистрибутивов (CLAUDE.md).

Код живёт в одном из четырёх модулей, и модуль импортирует только то, что
поедет вместе с ним: дистрибутив собирается каталогами, без вырезания.
"""
import ast
import os
import sys
import unittest

PACKAGE = os.path.dirname(os.path.dirname(os.path.dirname(os.path.realpath(__file__))))
ALLOWED = {
    "common": {"common"},
    "client": {"common", "client"},
    "server": {"common", "server"},
    "dev": {"common", "dev"},
}


def imports(path):
    """Имена, которые файл импортирует статически, абсолютными."""
    with open(path, encoding="utf-8") as f:
        tree = ast.parse(f.read())
    names = []
    for node in ast.walk(tree):
        if isinstance(node, ast.Import):
            names += [a.name for a in node.names]
        elif isinstance(node, ast.ImportFrom) and node.level == 0 and node.module:
            names.append(node.module)
            if node.module == "brood":
                names += [f"brood.{a.name}" for a in node.names]
    return names


def python_files(root):
    for top, dirs, files in os.walk(root):
        dirs[:] = [d for d in dirs if d != "__pycache__"]
        yield from (os.path.join(top, f) for f in files if f.endswith(".py"))


class LayoutTests(unittest.TestCase):
    def test_only_four_modules(self):
        entries = {e for e in os.listdir(PACKAGE) if e != "__pycache__"}
        self.assertEqual(entries, {"__init__.py", *ALLOWED})

    def test_imports_stay_inside_distribution(self):
        for module, allowed in ALLOWED.items():
            for path in python_files(os.path.join(PACKAGE, module)):
                with self.subTest(path=os.path.relpath(path, PACKAGE)):
                    used = {n.split(".")[1] for n in imports(path) if n.startswith("brood.")}
                    self.assertEqual(used - allowed, set())

    def test_common_takes_only_stdlib(self):
        """common едет в каждый дистрибутив и не тянет за собой зависимостей."""
        for path in python_files(os.path.join(PACKAGE, "common")):
            with self.subTest(path=os.path.relpath(path, PACKAGE)):
                tops = {n.split(".")[0] for n in imports(path)}
                self.assertEqual(tops - set(sys.stdlib_module_names) - {"brood"}, set())

    def test_no_relative_imports(self):
        """Относительный импорт прячет модуль от проверок выше."""
        for path in python_files(PACKAGE):
            with open(path, encoding="utf-8") as f:
                tree = ast.parse(f.read())
            lines = [n.lineno for n in ast.walk(tree) if isinstance(n, ast.ImportFrom) and n.level]
            self.assertEqual(lines, [], f"relative imports in {os.path.relpath(path, PACKAGE)}")
