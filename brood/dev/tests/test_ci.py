"""Чистая логика `brood dev ci` без GitLab: строки, отказы, «почему красный»."""
import unittest

from brood.dev import gitlab

URL = "https://git.example.dev/group/proj/-/pipelines/42"
JOB_URL = "https://git.example.dev/group/proj/-/jobs/7"


def job(**kw):
    return {"id": 7, "status": "success", "stage": "test", "name": "unit",
            "duration": 62.4, "allow_failure": False, "web_url": JOB_URL, **kw}


def pipe(**kw):
    return {"id": 42, "status": "failed", "sha": "cff9f6812345678901234567890abcdef0123456",
            "ref": "main", "created_at": "2026-09-24T10:11:12.345Z", "web_url": URL, **kw}


# Метка -- слово, а не значок; провал -- капсом.
MARKS = [("success", "ok"), ("failed", "FAILED"), ("running", "running"),
         ("canceled", "canceled"), (None, "?")]
# У ещё не стартовавшей джобы duration -- null.
DURATIONS = [(None, "-"), (0, "0s"), (59.9, "59s"), (60, "1m00s"), (62.4, "1m02s"),
             (3725, "62m05s")]
TAILS = [
    # Перевод строки в конце трассы строкой не считается.
    ("a\nb\nc\n", 2, "b\nc"),
    ("a\nb", 60, "a\nb"),
    ("a\nb\nc", None, "a\nb\nc"),
    ("a\nb", 0, ""),
    ("", 5, ""),
    (None, 5, ""),
]
# Терминал показывает от строки то, что после последнего \r: без этого
# хвост лога -- мусор маркеров секций.
SCREEN = [
    ("section_start:1695:step_script\r\x1b[0KExecuting step\nok\r\n",
     "\x1b[0KExecuting step\nok\n"),
    ("10%\r50%\r100%\ndone", "100%\ndone"),
    (None, ""),
]


class FormatTests(unittest.TestCase):
    def test_mark(self):
        for status, want in MARKS:
            with self.subTest(status=status):
                self.assertEqual(gitlab.mark(status), want)

    def test_duration(self):
        for secs, want in DURATIONS:
            with self.subTest(secs=secs):
                self.assertEqual(gitlab.duration(secs), want)

    def test_tail(self):
        for text, n, want in TAILS:
            with self.subTest(text=text, n=n):
                self.assertEqual(gitlab.tail(text, n), want)

    def test_screen_lines(self):
        for text, want in SCREEN:
            with self.subTest(text=text):
                self.assertEqual(gitlab.screen_lines(text), want)

    def test_lines(self):
        self.assertEqual(gitlab.pipeline_line(pipe()),
                         ("FAILED", "42", "cff9f681", "main", "2026-09-24 10:11", URL))
        self.assertEqual(gitlab.pipeline_line(pipe(created_at=None))[4], "-")
        self.assertEqual(gitlab.job_line(job()), ("ok", "7", "test", "unit", "1m02s", ""))
        self.assertEqual(gitlab.job_line(job(status="failed")),
                         ("FAILED", "7", "test", "unit", "1m02s", ""))
        # Терпимый провал -- не капсом: он пайплайн не валил.
        self.assertEqual(gitlab.job_line(job(status="failed", allow_failure=True)),
                         ("failed", "7", "test", "unit", "1m02s", "(allow_failure)"))

    def test_runner_lines(self):
        self.assertEqual(gitlab.runner_line(
            {"id": 5, "description": "hyper", "active": True, "paused": False,
             "status": "online", "tag_list": ["docker", "pve"]}),
            ("ON", "5", "hyper", "online", "docker,pve"))
        self.assertEqual(gitlab.runner_line(
            {"id": 6, "description": "", "paused": True, "status": "offline", "tag_list": []}),
            ("OFF", "6", "-", "offline", "-"))
        self.assertEqual(gitlab.running_line(
            job(status="running", ref="feat/1-x", runner={"id": 5, "description": "hyper"})),
            ("7", "test", "unit", "feat/1-x", "runner 5 hyper"))
        self.assertEqual(gitlab.running_line(job(status="running", ref="main", runner=None)),
                         ("7", "test", "unit", "main", "no runner"))

    def test_lint_report(self):
        self.assertEqual(gitlab.lint_report(
            {"valid": True, "errors": [], "warnings": ["jobs:x may run twice"],
             "jobs": [{"name": "unit"}, {"name": "e2e"}]}),
            (["valid, 2 job(s): unit, e2e", "warning: jobs:x may run twice"], True))
        self.assertEqual(gitlab.lint_report(
            {"valid": False, "errors": ["jobs:unit script missing"], "warnings": []}),
            (["invalid", "error: jobs:unit script missing"], False))


class RefusalTests(unittest.TestCase):
    def test_is_failure(self):
        self.assertTrue(gitlab.is_failure(job(status="failed")))
        self.assertFalse(gitlab.is_failure(job(status="failed", allow_failure=True)))
        self.assertFalse(gitlab.is_failure(job()))

    def test_running_refuses_retry(self):
        for status in ("running", "pending", "created"):
            with self.subTest(status=status):
                self.assertEqual(gitlab.retry_blocked(job(status=status)),
                                 f"job 7 is {status}: wait for it to finish before retrying")
                self.assertEqual(gitlab.pipeline_retry_blocked(pipe(status=status)),
                                 f"pipeline 42 is {status}: wait for it to finish before retrying")
                self.assertIsNone(gitlab.pipeline_cancel_blocked(pipe(status=status)))

    def test_finished_refuses_cancel(self):
        """GitLab принимает отмену законченного пайплайна и молча ничего не делает."""
        for status in ("success", "failed", "canceled", "skipped"):
            with self.subTest(status=status):
                self.assertEqual(gitlab.pipeline_cancel_blocked(pipe(status=status)),
                                 f"pipeline 42 is {status}: nothing to cancel")

    def test_finished_allows_retry(self):
        for status in ("failed", "canceled", "success"):
            with self.subTest(status=status):
                self.assertIsNone(gitlab.retry_blocked(job(status=status)))
                self.assertIsNone(gitlab.pipeline_retry_blocked(pipe(status=status)))


class WhyTests(unittest.TestCase):
    def test_pipeline_failure(self):
        self.assertEqual(gitlab.pipeline_failure(pipe(
            yaml_errors="jobs:unit config contains unknown keys: scrpt",
            detailed_status={"text": "failed"})),
            ["yaml_errors: jobs:unit config contains unknown keys: scrpt",
             "detailed_status: failed"])
        self.assertEqual(gitlab.pipeline_failure(pipe(failure_reason="config_error")),
                         ["failure_reason: config_error"])
        self.assertEqual(gitlab.pipeline_failure(pipe()), [])

    def test_failed_and_tolerated(self):
        failed = job(id=8, status="failed", name="e2e", duration=5,
                     web_url="https://git.example.dev/group/proj/-/jobs/8")
        allowed = job(id=9, status="failed", name="lint", allow_failure=True, duration=None)
        self.assertEqual(gitlab.why_lines(pipe(), [job(), failed, allowed],
                                          {8: "boom\nFAILED 1/3"}), [
            f"pipeline 42 failed: {URL}",
            "",
            "job 8 test/e2e failed (5s): https://git.example.dev/group/proj/-/jobs/8",
            "boom",
            "FAILED 1/3",
            "",
            "job 9 test/lint failed (allow_failure): did NOT fail the pipeline"])

    def test_pipeline_reason(self):
        # Отвергнутый .gitlab-ci.yml валит пайплайн без единой джобы.
        self.assertEqual(gitlab.why_lines(pipe(yaml_errors="bad include"), [], {}), [
            f"pipeline 42 failed: {URL}", "no jobs failed; the pipeline itself says:",
            "yaml_errors: bad include"])
        self.assertEqual(gitlab.why_lines(pipe(), [], {}), [
            f"pipeline 42 failed: {URL}", "no jobs failed, and GitLab gives no reason"])
        self.assertEqual(gitlab.why_lines(pipe(status="success"), [job()], {}), [
            f"pipeline 42 success: {URL}", "nothing failed"])
