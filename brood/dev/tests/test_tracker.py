"""Чистая логика трекера без GitLab: координаты, метки, эпики, ветки.

Ошибка здесь молчит: лишняя метка одной группы не отказ, GitLab оставит одну
из двух и не скажет какую, а задача выпадет из выборки, которой её ищут.
"""
import unittest
from unittest import mock

from brood.dev import gitlab

ORIGINS = [
    ("git@git.example.dev:group/proj.git", ("git.example.dev", "group/proj")),
    ("ssh://git@git.example.dev:2222/group/sub/proj.git", ("git.example.dev", "group/sub/proj")),
    ("https://git.example.dev/group/proj.git", ("git.example.dev", "group/proj")),
    ("https://git.example.dev/group/proj/", ("git.example.dev", "group/proj")),
    ("/srv/git/proj.git", ("", "")),
    ("./a:b", ("", "")),
    ("proj", ("", "")),
    ("", ("", "")),
]

BRANCHES = [
    (["bug", "sev::high"], 12, None, "bug/12"),
    (["bug"], 12, "Папет залипает на диалоге", "bug/12"),
    (["bug"], 12, "clone holds work", "bug/12-clone-holds-work"),
    (["feature"], 7, "driver seam", "feat/7-driver-seam"),
    (["type::docs"], 3, "BUS", "docs/3-bus"),
    # Дефект перевешивает тип.
    (["bug", "feature"], 9, None, "bug/9"),
    ([], 1, None, "feat/1"),
    (["type::refactor"], 5, "a" * 60, "refactor/5-" + "a" * 40),
]

STATUS = [
    (["status::live", "sev::low"], "wip", ["sev::low", "status::wip"]),
    (["status::wip"], "fixed", ["status::fixed"]),
    # Идемпотентность: повторный переход не кладёт вторую метку группы.
    (["status::wip"], "wip", ["status::wip"]),
    ([], "live", ["status::live"]),
]

# relabel только открывает: закрытие -- работа `brood dev bug close`.
RELABEL_ACTION = [
    ("closed", "parked", "reopen"),
    ("closed", "live", "reopen"),
    ("closed", "wip", "reopen"),
    ("closed", "fixed", "relabel"),
    ("closed", "noise", "relabel"),
    ("closed", "dup", "relabel"),
    ("closed", None, "relabel"),
    ("opened", "parked", "relabel"),
    ("opened", "fixed", "relabel"),
    ("opened", "live", "relabel"),
]

EPICS = [
    ("текст", 5, "**Эпик:** #5\n\nтекст"),
    ("**Эпик:** #5\n\nтекст", 5, "**Эпик:** #5\n\nтекст"),
    ("**Эпик:** #4\n\nтекст", 5, "**Эпик:** #5\n\nтекст"),
    ("", 7, "**Эпик:** #7"),
]

EPIC_OF = [
    ("**Эпик:** #5\n\nтекст", 5),
    # Только первой строкой: ссылка в теле -- не родство.
    ("текст\n\n**Эпик:** #5", None),
    ("текст", None),
    ("", None),
    (None, None),
]

OLD = "**Эпик:** #281\n\nстарое тело"
REPLACE_BODY = [
    (OLD, "новое тело", gitlab.KEEP, "**Эпик:** #281\n\nновое тело"),
    (OLD, "новое тело", 7, "**Эпик:** #7\n\nновое тело"),
    (OLD, "новое тело", None, "новое тело"),
    (OLD, "**Эпик:** #9\n\nсвоё", gitlab.KEEP, "**Эпик:** #9\n\nсвоё"),
    (OLD, "**Эпик:** #9\n\nсвоё", None, "своё"),
    ("голое тело", "новое", gitlab.KEEP, "новое"),
]


class TrackerTests(unittest.TestCase):
    def test_parse_origin(self):
        for url, want in ORIGINS:
            with self.subTest(url=url):
                self.assertEqual(gitlab.parse_origin(url), want)

    def test_branch_name(self):
        for labels, iid, slug, want in BRANCHES:
            with self.subTest(labels=labels, slug=slug):
                self.assertEqual(gitlab.branch_name(labels, iid, slug), want)

    def test_with_status(self):
        for labels, status, want in STATUS:
            with self.subTest(labels=labels, status=status):
                self.assertEqual(gitlab.with_status(labels, status), want)
        with self.assertRaisesRegex(RuntimeError, "unknown status 'partial'"):
            gitlab.with_status([], "partial")

    def test_relabel_action(self):
        for state, status, want in RELABEL_ACTION:
            with self.subTest(state=state, status=status):
                self.assertEqual(gitlab.relabel_action(state, status), want)

    def test_parked_is_open(self):
        """Закрытая отложенная пропадает из списков и выглядит сделанной."""
        self.assertIn("parked", gitlab.OPEN_STATUSES)
        self.assertNotIn("parked", gitlab.CLOSE_STATUSES)

    def test_check_label(self):
        self.assertEqual(gitlab.check_label("component::bus"), "component::bus")
        with self.assertRaisesRegex(RuntimeError, "one of: live, wip"):
            gitlab.check_label("status::nope")
        with self.assertRaisesRegex(RuntimeError, "unknown label group nope"):
            gitlab.check_label("nope::live")

    def test_epics(self):
        for body, iid, want in EPICS:
            with self.subTest(body=body, iid=iid):
                self.assertEqual(gitlab.with_epic(body, iid), want)
        for body, want in EPIC_OF:
            with self.subTest(body=body):
                self.assertEqual(gitlab.epic_of(body), want)

    def test_replace_body(self):
        for old, new, epic, want in REPLACE_BODY:
            with self.subTest(old=old, new=new, epic=epic):
                self.assertEqual(gitlab.replace_body(old, new, epic), want)

    def test_edit_payload(self):
        self.assertEqual(gitlab.edit_payload("Заголовок", None), {"title": "Заголовок"})
        self.assertEqual(gitlab.edit_payload(None, "тело"), {"description": "тело"})
        with self.assertRaises(ValueError):
            gitlab.edit_payload(None, None)

    def test_sev_key_orders_worst_first(self):
        issues = [{"iid": 3, "labels": []}, {"iid": 2, "labels": ["sev::low"]},
                  {"iid": 9, "labels": ["sev::high"]}, {"iid": 1, "labels": ["sev::high"]}]
        self.assertEqual([i["iid"] for i in sorted(issues, key=gitlab.sev_key)], [1, 9, 2, 3])


def settings(**values):
    return mock.patch.object(gitlab.config, "get", lambda name: values.get(name, ""))


class CoordinatesTests(unittest.TestCase):
    def test_auth_header(self):
        with settings(GITLAB_TOKEN="glpat-x"):
            self.assertEqual(gitlab.auth_header(), ("PRIVATE-TOKEN", "glpat-x"))
        with settings(), self.assertRaisesRegex(RuntimeError, "GITLAB_TOKEN"):
            gitlab.auth_header()

    def test_origin_gives_coordinates(self):
        with settings(), mock.patch.object(gitlab, "origin",
                                           lambda: "git@git.example.dev:group/proj.git"):
            self.assertEqual((gitlab.site(), gitlab.project()), ("git.example.dev", "group/proj"))

    def test_env_overrides_origin(self):
        with settings(GITLAB_SITE="tracker.example", GITLAB_PROJECT="other/proj"), \
                mock.patch.object(gitlab, "origin", lambda: "git@git.example.dev:group/proj.git"):
            self.assertEqual(gitlab.base(), "https://tracker.example/api/v4/projects/other%2Fproj")

    def test_no_coordinates_refuse(self):
        with settings(), mock.patch.object(gitlab, "origin", lambda: ""):
            with self.assertRaisesRegex(RuntimeError, "GITLAB_SITE"):
                gitlab.site()
            with self.assertRaisesRegex(RuntimeError, "GITLAB_PROJECT"):
                gitlab.project()
