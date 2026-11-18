"""Глаголы `brood dev`: дерево команд, разбор группы ci, улики слияния."""
import unittest

from brood.common import cli
from brood.dev.cli import ci
from brood.dev.cli.bug import evidence

# Без глагола -- список, с числом -- пайплайн: разбирает сама группа.
ROUTES = [
    ([], ("list", [])),
    (["42"], ("show", ["42"])),
    (["42", "--failed"], ("show", ["42", "--failed"])),
    (["nope"], None),
]

EVIDENCE_BRANCHES = [
    ("origin/bug/12", True),
    ("origin/feature/12-driver-seam", True),
    # Ветки задач до #297 сокращали feature до feat: их улики тоже ищутся.
    ("origin/feat/12-driver-seam", True),
    # Соседний номер -- чужая задача.
    ("origin/bug/123", False),
    ("origin/bug/1", False),
    ("origin/epic/12", False),
    ("origin/feat/12-Upper", False),
]

MERGES = [
    ("Merge branch 'bug/12-x' into 'main'", "bug/12-x"),
    ("Merge branch 'bug/12' into main", "bug/12"),
    # #номер в теме -- упоминание, а не слияние.
    ("fix: retry the job #12", None),
]


class DevCommandsTests(unittest.TestCase):
    def test_dev_verbs(self):
        tree = cli.groups()["dev"]
        self.assertEqual(set(tree), {"bug", "ci", "mr", "setup", "test", "web"})
        self.assertEqual(set(tree["mr"]), {"show", "merge"})
        self.assertEqual(set(tree["bug"]), {"close", "comment", "edit", "epic", "evidence",
                                            "labels", "list", "new", "relabel", "show", "start"})
        self.assertEqual(set(tree["ci"]), {"artifact", "cancel", "lint", "list", "log", "retry",
                                           "runners", "set", "show", "vars", "why"})

    def test_ci_route(self):
        for argv, want in ROUTES:
            with self.subTest(argv=argv):
                self.assertEqual(ci.route(argv), want)

    def test_evidence_branch(self):
        for ref, want in EVIDENCE_BRANCHES:
            with self.subTest(ref=ref):
                self.assertIs(evidence.evidence_branch(ref, 12), want)

    def test_merge_source(self):
        for subject, want in MERGES:
            with self.subTest(subject=subject):
                self.assertEqual(evidence.merge_source(subject), want)
