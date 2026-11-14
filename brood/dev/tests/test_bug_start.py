"""`brood dev bug start`: заметка на задаче и вывод называют ту ветку, которую
заведут под этим именем (#297).

Имя -- по правилу «Ветка задачи» в CLAUDE.md: слово метки `type::`, номер,
слаг из ASCII-слов заголовка либо из аргумента `slug`. С --no-branch ветку
заводит исполнитель ровно под напечатанным именем.
"""
import contextlib
import http.client
from http.server import BaseHTTPRequestHandler, HTTPServer
import io
import json
from pathlib import Path
import re
import shlex
import subprocess
import tempfile
import threading
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
    with gitlab.project_scope("group/project"), \
            mock.patch.object(gitlab, "require_local_project", return_value="/checkout"), \
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


class StartFailureTests(unittest.TestCase):
    # BUG: Отказ checkout оставляет wip и заметку о несуществующей ветке (#301).
    # HYPOTHESIS: Запись в трекер идёт до checkout, а частичный успех не описан.
    # SOLUTION: Checkout до трекера; при отказе API сохраняем ветку и даём путь восстановления.
    # RESULT: FIXED — отказ checkout не меняет трекер; частичный успех API можно восстановить.
    # STATUS: FIXED
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name) / "checkout"
        self.root.mkdir()
        self.git("init", "-q", "-b", "main")
        self.git("config", "user.name", "Test")
        self.git("config", "user.email", "test@example.com")
        self.file = self.root / "tracked"
        self.file.write_text("main\n")
        self.git("add", "tracked")
        self.git("commit", "-qm", "initial")
        remote = Path(temp.name) / "origin.git"
        self.git("clone", "-q", "--bare", str(self.root), str(remote))
        self.git("remote", "add", "origin", str(remote))
        self.git("checkout", "-qb", "working")
        self.file.write_text("working\n")
        self.git("commit", "-qam", "working version")
        self.labels = list(ISSUE["labels"])
        self.notes = []
        self.out = io.StringIO()
        self.enterContext(contextlib.redirect_stdout(self.out))
        self.enterContext(mock.patch.object(gitlab, "require_local_project",
                                           return_value=str(self.root)))
        self.enterContext(mock.patch.object(gitlab, "issue", side_effect=lambda iid: {
            "labels": list(self.labels), "title": ISSUE["title"], "state": "opened"}))
        self.relabel = self.enterContext(mock.patch.object(
            gitlab, "relabel", side_effect=lambda iid, labels: self.labels.__setitem__(slice(None), labels)))
        self.comment = self.enterContext(mock.patch.object(
            gitlab, "comment", side_effect=lambda iid, body: self.notes.append(body)))

    def git(self, *args):
        return subprocess.run(["git", "-C", str(self.root), *args],
                              check=True, capture_output=True, text=True).stdout.strip()

    def test_failed_checkout_leaves_issue_untouched(self):
        self.file.write_text("uncommitted work\n")
        with self.assertRaisesRegex(SystemExit, "local changes.*overwritten"):
            start.main(["285"])
        self.relabel.assert_not_called()
        self.comment.assert_not_called()
        self.assertEqual(self.labels, ISSUE["labels"])
        self.assertEqual(self.notes, [])
        self.assertEqual(self.out.getvalue(), "")
        self.assertEqual(self.git("branch", "--show-current"), "working")
        self.assertEqual(self.file.read_text(), "uncommitted work\n")
        self.assertEqual(self.git("branch", "--list", BRANCH), "")
        self.assertEqual(self.git("ls-remote", "origin", "refs/heads/" + BRANCH), "")

        # После устранения конфликта обычный повтор создаёт ветку и одну заметку.
        self.git("restore", "tracked")
        start.main(["285"])
        self.assertEqual(self.git("branch", "--show-current"), BRANCH)
        self.assertEqual(self.labels, ["type::feature", "status::wip"])
        self.assertEqual(self.notes, [f"Работа начата в ветке `{BRANCH}`."])
        self.assertEqual(self.out.getvalue(), f"#285 claimed, on {BRANCH}\n")

    def assert_recovery(self, message):
        self.assertIn("brood dev bug show 285", message)
        self.assertIn("brood dev bug relabel 285 --status wip", message)
        self.assertIn("brood dev bug comment 285 --body-file -", message)

    def test_relabel_failure_preserves_branch_and_explains_recovery(self):
        for failure in (RuntimeError("tracker unavailable"), OSError("connection lost")):
            with self.subTest(failure=failure):
                self.relabel.side_effect = failure
                with self.assertRaises(RuntimeError) as caught:
                    start.main(["285"])
                message = str(caught.exception)
                self.assertIn(str(failure), message)
                self.assertIn("claim not confirmed", message)
                self.assertIn(f"checkout is on {BRANCH}", message)
                self.assert_recovery(message)
                self.comment.assert_not_called()
                self.assertEqual(self.labels, ISSUE["labels"])
                self.assertEqual(self.notes, [])
                self.assertEqual(self.out.getvalue(), "")
                self.assertEqual(self.git("branch", "--show-current"), BRANCH)
                self.assertEqual(self.file.read_text(), "main\n")
                self.git("checkout", "working")
                self.git("branch", "-D", BRANCH)

    def test_comment_failure_reports_confirmed_claim_and_can_be_recovered(self):
        from brood.dev.cli.bug import comment

        self.comment.side_effect = RuntimeError("note refused")
        with self.assertRaises(RuntimeError) as caught:
            start.main(["285"])
        message = str(caught.exception)
        self.assertIn("note refused", message)
        self.assertIn("status::wip set; note not confirmed", message)
        self.assertIn(f"checkout is on {BRANCH}", message)
        self.assert_recovery(message)
        self.assertEqual(self.labels, ["type::feature", "status::wip"])
        self.assertEqual(self.notes, [])
        self.assertEqual(self.out.getvalue(), "")
        self.assertEqual(self.git("branch", "--show-current"), BRANCH)

        # Повтор не должен скрывать частичный успех за голым «ветка существует».
        self.relabel.reset_mock()
        self.comment.reset_mock()
        with self.assertRaises(RuntimeError) as caught:
            start.main(["285"])
        self.assertIn("branch already exists", str(caught.exception))
        self.assert_recovery(str(caught.exception))
        self.relabel.assert_not_called()
        self.comment.assert_not_called()

        # Указанный путь восстановления дописывает только отсутствующую заметку.
        self.comment.side_effect = lambda iid, body: self.notes.append(body)
        with mock.patch("sys.stdin", io.StringIO(f"Работа начата в ветке `{BRANCH}`.")):
            comment.main(["285", "--body-file", "-"])
        self.assertEqual(self.notes, [f"Работа начата в ветке `{BRANCH}`."])
        self.assertEqual(self.git("branch", "--show-current"), BRANCH)

    def test_relabel_recovery_uses_tracker_commands_without_recreating_branch(self):
        from brood.dev.cli.bug import comment, relabel

        self.relabel.side_effect = RuntimeError("relabel refused")
        with self.assertRaises(RuntimeError) as caught:
            start.main(["285"])
        self.assert_recovery(str(caught.exception))
        self.relabel.side_effect = lambda iid, labels: self.labels.__setitem__(slice(None), labels)
        relabel.main(["285", "--status", "wip"])
        with mock.patch("sys.stdin", io.StringIO(f"Работа начата в ветке `{BRANCH}`.")):
            comment.main(["285", "--body-file", "-"])
        self.assertEqual(self.labels, ["type::feature", "status::wip"])
        self.assertEqual(self.notes, [f"Работа начата в ветке `{BRANCH}`."])
        self.assertEqual(self.git("branch", "--show-current"), BRANCH)

    def test_dispatcher_reports_partial_failure_without_traceback(self):
        from brood.common.cli import main

        self.relabel.side_effect = RuntimeError("tracker unavailable")
        err = io.StringIO()
        with contextlib.redirect_stderr(err):
            result = main(["dev", "bug", "start", "285"])
        self.assertEqual(result, 1)
        self.assertIn("claim not confirmed", err.getvalue())
        self.assertIn(f"checkout is on {BRANCH}", err.getvalue())
        self.assert_recovery(err.getvalue())
        self.assertNotIn("Traceback", err.getvalue())
        self.assertEqual(self.out.getvalue(), "")
        self.assertEqual(self.git("branch", "--show-current"), BRANCH)

    def test_lost_relabel_reply_does_not_claim_tracker_was_unchanged(self):
        def apply_then_fail(iid, labels):
            self.labels[:] = labels
            raise RuntimeError("reply lost")

        self.relabel.side_effect = apply_then_fail
        with self.assertRaises(RuntimeError) as caught:
            start.main(["285"])
        self.assertIn("claim not confirmed", str(caught.exception))
        self.assert_recovery(str(caught.exception))
        self.assertEqual(self.labels, ["type::feature", "status::wip"])
        self.assertEqual(self.notes, [])
        self.comment.assert_not_called()
        self.assertEqual(self.git("branch", "--show-current"), BRANCH)

    # BUG: Оборванный ответ после записи обходит восстановление и диспетчер (#301).
    # HYPOTHESIS: urllib бросает IncompleteRead, который не является OSError.
    # SOLUTION: Библиотека переводит оборванный ответ в RuntimeError без повтора записи.
    # RESULT: FIXED — обе записи дают однострочный отказ с восстановлением, без дубликатов.
    # STATUS: FIXED
    def test_truncated_relabel_response_reports_uncertain_claim(self):
        self.assert_truncated_response("PUT", "claim not confirmed", expected_notes=0)

    def test_truncated_comment_response_reports_confirmed_claim(self):
        self.assert_truncated_response("POST", "status::wip set; note not confirmed", expected_notes=1)

    # BUG: Чтение оборванного тела HTTP 500 обходит обработку IncompleteRead (#301).
    # HYPOTHESIS: Исключение из HTTPError.read не попадает в соседний except.
    # SOLUTION: Одно чтение нормализует обрыв тела успешного ответа и HTTPError.
    # RESULT: FIXED — HTTP 500 после PUT/POST даёт восстановление без утечки и повтора.
    # STATUS: FIXED
    def test_truncated_error_relabel_response_reports_uncertain_claim(self):
        self.assert_truncated_response("PUT", "claim not confirmed", expected_notes=0, status=500)

    def test_truncated_error_comment_response_reports_confirmed_claim(self):
        self.assert_truncated_response("POST", "status::wip set; note not confirmed",
                                       expected_notes=1, status=500)

    def assert_truncated_response(self, truncate_method, state, expected_notes, status=200):
        from brood.common.cli import main

        writes = []
        owner = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def mutate(self):
                payload = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                writes.append(self.command)
                if self.command == "PUT":
                    owner.labels[:] = payload["labels"].split(",")
                else:
                    owner.notes.append(payload["body"])
                body = b'{"secret": "response-secret-must-not-leak"}'
                self.send_response(status if self.command == truncate_method else 200)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body[:-2] if self.command == truncate_method else body)
                self.wfile.flush()
                self.close_connection = True

            do_PUT = mutate
            do_POST = mutate

        server = HTTPServer(("127.0.0.1", 0), Handler)
        worker = threading.Thread(target=server.serve_forever, daemon=True)
        worker.start()
        self.relabel.side_effect = lambda iid, labels: gitlab.call(
            "PUT", f"/issues/{iid}", {"labels": ",".join(labels)})
        self.comment.side_effect = lambda iid, body: gitlab.call(
            "POST", f"/issues/{iid}/notes", {"body": body})
        err = io.StringIO()
        try:
            with mock.patch.object(gitlab, "base", return_value=f"http://127.0.0.1:{server.server_port}"), \
                    mock.patch.object(gitlab, "auth_header", return_value=("X-Test", "dummy")), \
                    contextlib.redirect_stderr(err):
                try:
                    result = main(["dev", "bug", "start", "285"])
                except http.client.IncompleteRead:
                    self.fail("IncompleteRead escaped the dispatcher after the tracker write")
            self.assertEqual(result, 1)
            message = err.getvalue()
            self.assertIn(state, message)
            self.assertIn("incomplete response", message)
            self.assertIn(f"{truncate_method} /issues/285", message)
            self.assertIn(f"checkout is on {BRANCH}", message)
            self.assert_recovery(message)
            self.assertNotIn("Traceback", message)
            self.assertNotIn("response-secret-must-not-leak", message)
            self.assertEqual(len(message.splitlines()), 1)
            self.assertEqual(self.labels, ["type::feature", "status::wip"])
            self.assertEqual(len(self.notes), expected_notes)
            self.assertEqual(self.out.getvalue(), "")
            self.assertEqual(self.git("branch", "--show-current"), BRANCH)
            self.assertEqual(writes, ["PUT"] if truncate_method == "PUT" else ["PUT", "POST"])

            # Повтор start не повторяет запись с потерянным ответом.
            with contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(main(["dev", "bug", "start", "285"]), 1)
            self.assertEqual(writes, ["PUT"] if truncate_method == "PUT" else ["PUT", "POST"])
            self.assertEqual(len(self.notes), expected_notes)
        finally:
            server.shutdown()
            server.server_close()
            worker.join()

    # BUG: Команды восстановления без --project меняют проект по умолчанию (#301).
    # HYPOTHESIS: Выбранный проект теряется при выходе из области диспетчера.
    # SOLUTION: Все три команды несут разрешённый проект с shell-экранированием.
    # RESULT: FIXED — диспетчер направляет восстановление в исходный проект при другом дефолте.
    # STATUS: FIXED
    def test_no_branch_recovery_commands_keep_selected_project(self):
        self.assert_project_recovery("relabel", no_branch=True)

    def test_local_branch_recovery_commands_keep_selected_project(self):
        self.assert_project_recovery("relabel")

    def test_comment_recovery_commands_keep_selected_project(self):
        self.assert_project_recovery("comment")

    def test_existing_branch_recovery_commands_keep_selected_project(self):
        self.git("branch", BRANCH)
        self.assert_project_recovery("branch")

    def test_default_project_is_explicit_in_recovery_commands(self):
        self.assert_project_recovery("relabel", no_branch=True, explicit=False)

    # BUG: GITLAB_PROJECT=123 допустим, но напечатанный --project 123 отвергается (#301).
    # HYPOTHESIS: Область --project принимает только путь, в отличие от конфигурации.
    # SOLUTION: Область принимает положительный числовой ID, как GitLab API.
    # RESULT: FIXED — три напечатанные команды выполняются с ID после смены дефолта.
    # STATUS: FIXED
    def test_numeric_configured_project_recovery_commands_are_executable(self):
        self.assert_project_recovery("relabel", no_branch=True, explicit=False, selected="123")

    def test_numeric_configured_project_comment_recovery_is_executable(self):
        self.assert_project_recovery("comment", no_branch=True, explicit=False, selected="123")

    def assert_project_recovery(self, failure, no_branch=False, explicit=True, selected="other/repo"):
        from brood.common.cli import main

        targets = []

        def issue(iid):
            targets.append(("show", gitlab.project()))
            return {"iid": int(iid), "labels": list(self.labels), "title": ISSUE["title"],
                    "state": "opened", "web_url": f"https://tracker.example/{selected}/-/issues/{iid}"}

        def relabel(iid, labels):
            targets.append(("relabel", gitlab.project()))
            self.labels[:] = labels

        def comment(iid, body):
            targets.append(("comment", gitlab.project()))
            self.notes.append(body)

        settings = {"GITLAB_PROJECT": "default/repo" if explicit else selected}
        self.enterContext(mock.patch.object(gitlab.config, "get", side_effect=lambda key: settings.get(key, "")))
        gitlab.issue.side_effect = issue
        self.relabel.side_effect = RuntimeError("tracker unavailable") if failure == "relabel" else relabel
        self.comment.side_effect = RuntimeError("note unavailable") if failure == "comment" else comment
        args = ["dev", "bug", "start", "285"]
        if explicit:
            args.extend(["--project", selected])
        if no_branch:
            args.append("--no-branch")
        err = io.StringIO()
        with contextlib.redirect_stderr(err):
            self.assertEqual(main(args), 1)
        if failure == "branch":
            self.assertIn("branch already exists", err.getvalue())
        settings["GITLAB_PROJECT"] = "default/repo"
        self.assertEqual(gitlab.project(), "default/repo")
        self.assertEqual(targets[0], ("show", selected))
        commands = re.findall(r"'(brood dev bug [^']+)'", err.getvalue())
        self.assertEqual(len(commands), 3)
        targets.clear()
        self.relabel.side_effect = relabel
        self.comment.side_effect = comment
        for command in commands:
            with mock.patch("sys.stdin", io.StringIO(f"Работа начата в ветке `{BRANCH}`.")):
                self.assertEqual(main(shlex.split(command)[1:]), 0)
            self.assertEqual(gitlab.project(), "default/repo")
        self.assertEqual(targets, [("show", selected), ("show", selected),
                                   ("relabel", selected), ("comment", selected)])
        self.assertEqual(self.labels, ["type::feature", "status::wip"])
        self.assertEqual(len(self.notes), 1)
        self.assertEqual(self.git("branch", "--show-current"), "working" if no_branch or failure == "branch" else BRANCH)

    def test_no_branch_failure_does_not_claim_local_checkout_changed(self):
        self.relabel.side_effect = RuntimeError("relabel refused")
        with self.assertRaises(RuntimeError) as caught:
            start.main(["285", "--no-branch"])
        message = str(caught.exception)
        self.assertIn("claim not confirmed", message)
        self.assert_recovery(message)
        self.assertNotIn("checkout is on", message)
        self.assertEqual(self.git("branch", "--show-current"), "working")
        self.assertEqual(self.git("branch", "--list", BRANCH), "")
        self.comment.assert_not_called()


if __name__ == "__main__":
    unittest.main()
