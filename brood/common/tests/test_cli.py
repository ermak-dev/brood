"""Диспетчер: обход модулей, спуск по глаголам, справка, отказы строкой."""
import contextlib
import io
import os
import unittest

from brood.common import cli

TREE = {"dev": {"bug": {"list": {}, "new": {}}, "ci": {"show": {}}}}

RESOLVE = [
    (["dev"], ("brood.dev.cli", [])),
    (["dev", "bug"], ("brood.dev.cli.bug", [])),
    (["dev", "bug", "list", "--all"], ("brood.dev.cli.bug.list", ["--all"])),
    # Глагол без своего модуля отвечает пакет над ним и получает argv.
    (["dev", "ci", "42", "--failed"], ("brood.dev.cli.ci", ["42", "--failed"])),
    (["dev", "nope"], ("brood.dev.cli", ["nope"])),
    # Модуль -- дно: слово после него -- аргумент, а не глагол.
    (["dev", "bug", "list", "new"], ("brood.dev.cli.bug.list", ["new"])),
    (["server"], None),
    ([], None),
]


def verb_files(words, tree):
    """Каждая команда группы -> (слова команды, файл её модуля)."""
    base = os.path.join(cli.PACKAGE, words[0], "cli", *words[1:])
    yield words, os.path.join(base, "__init__.py")
    for verb, sub in tree.items():
        if sub or os.path.isdir(os.path.join(base, verb)):
            yield from verb_files(words + [verb], sub)
        else:
            yield words + [verb], os.path.join(base, verb + ".py")


class DispatchTests(unittest.TestCase):
    def test_resolve(self):
        for argv, want in RESOLVE:
            with self.subTest(argv=argv):
                self.assertEqual(cli.resolve(argv, TREE), want)

    def test_dispatcher_is_not_a_group(self):
        self.assertNotIn("common", cli.groups())

    def test_every_command_has_usage(self):
        """Докстринг -- справка команды; у глагола он начинается с команды."""
        for group, tree in cli.groups().items():
            for words, path in verb_files([group], tree):
                with self.subTest(path=path):
                    doc = cli.docstring(path)
                    self.assertTrue(doc, "no docstring")
                    if not path.endswith("__init__.py"):
                        self.assertTrue(doc.startswith("brood " + " ".join(words)), doc)

    def test_help_lists_groups(self):
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            self.assertEqual(cli.main([]), 0)
        for group in cli.groups():
            self.assertIn(f"  {group:<10} ", out.getvalue())

    def test_unknown_command(self):
        err = io.StringIO()
        with contextlib.redirect_stderr(err):
            self.assertEqual(cli.main(["nope"]), 1)
        self.assertEqual(err.getvalue(), "brood: unknown command 'nope'; run 'brood help'\n")


class RunTests(unittest.TestCase):
    def test_refusal_is_one_line(self):
        for error in (RuntimeError("no GitLab site"), OSError("no route"),
                      ImportError("no module")):
            with self.subTest(error=error):
                def fn(argv, error=error):
                    raise error
                err = io.StringIO()
                with contextlib.redirect_stderr(err):
                    self.assertEqual(cli.run(fn, []), 1)
                self.assertEqual(err.getvalue(), f"{error}\n")

    def test_exit_code(self):
        self.assertEqual(cli.run(lambda argv: None, []), 0)
        self.assertEqual(cli.run(lambda argv: 3, []), 3)
