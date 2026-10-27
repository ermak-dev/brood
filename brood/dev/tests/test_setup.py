"""`brood dev setup`: зависимости из pyproject.toml и дописка .env."""
import tomllib
import unittest

from brood.dev.cli import setup

PROJECT = {"project": {"optional-dependencies": {
    "dev": ["ruff==0.15.16"], "other": ["x"]}}}

MISSING = [
    ({}, [("BROOD_SESSION_SECRET", "k")]),
    ({"BROOD_SESSION_SECRET": "old"}, []),
    ({"BROOD_SESSION_SECRET": ""}, [("BROOD_SESSION_SECRET", "k")]),
    ({"DJANGO_SECRET_KEY": "old"}, [("BROOD_SESSION_SECRET", "k")]),
]

APPEND = [
    ("", [("A", "1")], "A='1'\n"),
    ("X=1\n", [("A", "1"), ("B", "2")], "A='1'\nB='2'\n"),
    # Без перевода строки в конце файла новая строка склеилась бы со старой.
    ("X=1", [("A", "1")], "\nA='1'\n"),
]


class SetupTests(unittest.TestCase):
    def test_requirements(self):
        self.assertEqual(setup.requirements(PROJECT), ["ruff==0.15.16"])
        with self.assertRaisesRegex(RuntimeError, "no extra dev"):
            setup.requirements({"project": {"optional-dependencies": {}}})

    def test_declared_extras(self):
        """Extras рабочей копии объявлены в настоящем pyproject.toml."""
        with open(setup.PYPROJECT, "rb") as f:
            self.assertTrue(setup.requirements(tomllib.load(f)))

    def test_missing_settings(self):
        for env, want in MISSING:
            with self.subTest(env=env):
                self.assertEqual(setup.missing_settings(env, "k"), want)

    def test_append_lines(self):
        for text, pairs, want in APPEND:
            with self.subTest(text=text):
                self.assertEqual(setup.append_lines(text, pairs), want)
