"""`brood dev setup`: зависимости из pyproject.toml и дописка .env."""
import tomllib
import unittest

from brood.dev.cli import setup

PROJECT = {"project": {"optional-dependencies": {
    "server": ["Django==5.2.18"], "dev": ["ruff==0.15.16"], "other": ["x"]}}}

MISSING = [
    ({}, [("DJANGO_SECRET_KEY", "k"), ("DJANGO_DEBUG", "1")]),
    ({"DJANGO_SECRET_KEY": "old", "DJANGO_DEBUG": "0"}, []),
    # Пустой ключ -- то же, что нет его.
    ({"DJANGO_SECRET_KEY": "", "DJANGO_DEBUG": "1"}, [("DJANGO_SECRET_KEY", "k")]),
    # Названный, хоть и пустой, DJANGO_DEBUG -- решение человека.
    ({"DJANGO_SECRET_KEY": "old", "DJANGO_DEBUG": ""}, []),
]

APPEND = [
    ("", [("A", "1")], "A='1'\n"),
    ("X=1\n", [("A", "1"), ("B", "2")], "A='1'\nB='2'\n"),
    # Без перевода строки в конце файла новая строка склеилась бы со старой.
    ("X=1", [("A", "1")], "\nA='1'\n"),
]


class SetupTests(unittest.TestCase):
    def test_requirements(self):
        self.assertEqual(setup.requirements(PROJECT), ["Django==5.2.18", "ruff==0.15.16"])
        with self.assertRaisesRegex(RuntimeError, "no extra server"):
            setup.requirements({"project": {"optional-dependencies": {"dev": []}}})

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
