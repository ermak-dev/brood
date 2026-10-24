"""Чтение .env: формат mop, окружение старше файла."""
import os
import re
import tempfile
import unittest
from unittest import mock

from brood.common import config

PARSE = [
    # .env mop хранит значения в кавычках: кавычка -- часть формата.
    ("GITLAB_SITE='git.ermak.dev'", {"GITLAB_SITE": "git.ermak.dev"}),
    ('GITLAB_SITE="git.ermak.dev"', {"GITLAB_SITE": "git.ermak.dev"}),
    # Ключ отделяется первым `=`, остальные -- часть значения.
    ("TOKEN=a=b=c", {"TOKEN": "a=b=c"}),
    ("TOKEN='a=b'", {"TOKEN": "a=b"}),
    # Непарная кавычка -- значение как есть.
    ("X='abc", {"X": "'abc"}),
    ("X='", {"X": "'"}),
    ("  X = 1  ", {"X": "1"}),
    ("# X=1\n\nnot a pair\nY=2", {"Y": "2"}),
    ("X=1\nX=2", {"X": "2"}),
    ("X=", {"X": ""}),
]


class ParseEnvTests(unittest.TestCase):
    def test_parse_env(self):
        for text, want in PARSE:
            with self.subTest(text=text):
                self.assertEqual(config.parse_env(text), want)


class ReadEnvTests(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.dir = tmp.name

    def test_missing_file(self):
        self.assertEqual(config.read_env(os.path.join(self.dir, "absent")), {})

    def test_unreadable_refuses_with_path(self):
        with self.assertRaisesRegex(config.ReadError, re.escape(self.dir)):
            config.read_env(self.dir)

    def test_environment_over_file(self):
        path = os.path.join(self.dir, ".env")
        with open(path, "w") as f:
            f.write("GITLAB_SITE='file.example'\nGITLAB_PROJECT=group/proj\n")
        with mock.patch.dict(os.environ, {"GITLAB_SITE": "env.example"}):
            os.environ.pop("GITLAB_PROJECT", None)
            os.environ.pop("GITLAB_TOKEN", None)
            self.assertEqual(config.get("GITLAB_SITE", path), "env.example")
            self.assertEqual(config.get("GITLAB_PROJECT", path), "group/proj")
            self.assertEqual(config.get("GITLAB_TOKEN", path), "")
