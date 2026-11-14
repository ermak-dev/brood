"""`brood dev bug start`: заметка на задаче и вывод называют ту ветку, которую
заведут под этим именем (#297).

Имя -- по правилу «Ветка задачи» в CLAUDE.md: слово метки `type::`, номер,
слаг из ASCII-слов заголовка либо из аргумента `slug`. С --no-branch ветку
заводит исполнитель ровно под напечатанным именем.
"""
import contextlib
import io
import unittest
from unittest import mock

from brood.dev import gitlab
from brood.dev.cli.bug import start

# Настоящая #285: из русского заголовка в слаг попадает только `telegram`.
ISSUE = {"labels": ["type::feature", "status::live"],
         "title": "Панель: секция Telegram в профиле пользователя"}
# Ветки по правилу CLAUDE.md, а не вычисленные branch_name: тест держит
# правило, а не повторяет реализацию.
BRANCH = "feature/285-telegram"
# Другой слаг исполнитель не придумывает сам, а передаёт аргументом.
SLUG_BRANCH = "feature/285-telegram-panel"


@contextlib.contextmanager
def tracker():
    """Командлет без GitLab и без git: моки relabel, comment, git и вывод."""
    out = io.StringIO()
    with mock.patch.object(gitlab, "require_local_project", return_value="/checkout"), \
            mock.patch.object(gitlab, "issue", return_value=ISSUE), \
            mock.patch.object(gitlab, "relabel") as relabel, \
            mock.patch.object(gitlab, "comment") as comment, \
            mock.patch.object(start, "git") as git, \
            mock.patch.object(start, "default_branch", return_value="origin/main"), \
            mock.patch.object(start.subprocess, "run", return_value=mock.Mock(returncode=1)), \
            contextlib.redirect_stdout(out):
        yield relabel, comment, git, out


class StartNoteTests(unittest.TestCase):
    def test_note_names_created_branch(self):
        with tracker() as (relabel, comment, git, out):
            start.main(["285", "--worker", "brood-7"])
        self.assertIn(mock.call("checkout", "-b", BRANCH, "origin/main", cwd="/checkout"),
                      git.call_args_list)
        comment.assert_called_once_with(
            "285", f"Работа начата в ветке `{BRANCH}` · воркер brood-7.")
        relabel.assert_called_once_with("285", ["type::feature", "status::wip"])
        self.assertEqual(out.getvalue(), f"#285 claimed, on {BRANCH}\n")

    def test_no_branch_note_names_executor_branch(self):
        with tracker() as (relabel, comment, git, out):
            start.main(["285", "--no-branch", "--worker", "brood-7"])
        git.assert_not_called()
        comment.assert_called_once_with(
            "285", f"Работа начата; ветку `{BRANCH}` заведёт исполнитель · воркер brood-7.")
        relabel.assert_called_once_with("285", ["type::feature", "status::wip"])
        self.assertEqual(out.getvalue(), f"#285 claimed; branch for the executor: {BRANCH}\n")

    def test_slug_argument_names_executor_branch(self):
        with tracker() as (_relabel, comment, git, out):
            start.main(["285", "telegram-panel", "--no-branch"])
        git.assert_not_called()
        comment.assert_called_once_with(
            "285", f"Работа начата; ветку `{SLUG_BRANCH}` заведёт исполнитель.")
        self.assertEqual(out.getvalue(),
                         f"#285 claimed; branch for the executor: {SLUG_BRANCH}\n")


if __name__ == "__main__":
    unittest.main()
