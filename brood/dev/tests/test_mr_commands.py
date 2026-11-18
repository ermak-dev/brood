import contextlib
import http.client
import io
import json
import unittest
import urllib.error
from unittest import mock

from brood.common.cli import main
from brood.dev import gitlab
from brood.dev.tests.test_merge_requests import SHA, OTHER, SECRET, mr


class MRCommandsTests(unittest.TestCase):
    def invoke(self, args, responses, settings=None):
        requests = []
        values = {"GITLAB_SITE": "git.example", "GITLAB_TOKEN": "synthetic-token",
                  **(settings or {})}
        pending = iter(responses)

        def open_request(request, **kwargs):
            requests.append(request)
            response = next(pending)
            if isinstance(response, Exception):
                raise response
            raw = response if isinstance(response, bytes) else json.dumps(response).encode()
            return contextlib.closing(io.BytesIO(raw))

        output, errors = io.StringIO(), io.StringIO()
        with mock.patch.object(gitlab.config, "get", side_effect=values.get), \
                mock.patch.object(gitlab, "origin", return_value="git@git.example:origin/project.git"), \
                mock.patch("urllib.request.urlopen", side_effect=open_request), \
                contextlib.redirect_stdout(output), contextlib.redirect_stderr(errors):
            try:
                code = main(["dev", "mr", *args])
            except SystemExit as error:
                code = 1 if error.code else 0
                errors.write(str(error.code))
        return code, output.getvalue(), errors.getvalue(), requests

    def test_show_is_get_only_in_default_origin_project(self):
        code, output, errors, requests = self.invoke(["show", "316"], [mr()])
        self.assertEqual((code, errors), (0, ""))
        self.assertIn("opened", output)
        self.assertIn(SHA, output)
        self.assertEqual([(r.method, r.full_url, r.data) for r in requests], [
            ("GET", "https://git.example/api/v4/projects/origin%2Fproject/merge_requests/316", None)])
        self.assertEqual(requests[0].get_header("Private-token"), "synthetic-token")

    def test_explicit_and_environment_project_scope(self):
        for args, project in ((["--project", "other/project", "show", "316"], "other%2Fproject"),
                              (["show", "316", "--project=123"], "123"),
                              (["show", "316"], "env%2Fproject")):
            with self.subTest(args=args):
                code, _, errors, requests = self.invoke(args, [mr()], {"GITLAB_PROJECT": "env/project"})
                self.assertEqual((code, errors), (0, ""))
                self.assertEqual(requests[0].full_url,
                                 f"https://git.example/api/v4/projects/{project}/merge_requests/316")
        code, _, _, requests = self.invoke(["show", "316"], [mr()])
        self.assertEqual(code, 0)
        self.assertIn("/origin%2Fproject/", requests[0].full_url)

    def test_merge_exact_scope_and_payload_and_null_merge_sha(self):
        for extra in ([], ["--sha", SHA]):
            code, output, errors, requests = self.invoke(
                ["merge", "316", *extra, "--project", "other/project"],
                [mr(), mr(state="merged")])
            self.assertEqual((code, errors), (0, ""))
            self.assertIn("API reports merged", output)
            self.assertIn("Git/main not verified", output)
            self.assertEqual([r.method for r in requests], ["GET", "PUT"])
            self.assertEqual([r.full_url for r in requests], [
                "https://git.example/api/v4/projects/other%2Fproject/merge_requests/316",
                "https://git.example/api/v4/projects/other%2Fproject/merge_requests/316/merge"])
            self.assertEqual(json.loads(requests[1].data),
                             {"squash": True, "auto_merge": False, "sha": SHA})

    def test_sha_mismatch_never_puts(self):
        code, _, errors, requests = self.invoke(["merge", "316", "--sha", OTHER], [mr()])
        self.assertEqual(code, 1)
        self.assertIn("mismatch", errors)
        self.assertEqual([r.method for r in requests], ["GET"])

    def test_noncompletion_response_not_reported_as_merged(self):
        code, output, _, requests = self.invoke(["merge", "316"], [mr(), mr(auto_merge=True)])
        self.assertEqual(code, 1)
        self.assertIn("not confirmed", output)
        self.assertNotIn("API reports merged", output)
        self.assertIn("mr show 316", output)
        self.assertEqual(len(requests), 2)

    def test_known_refusals_do_not_echo_http_body_or_retry(self):
        for status in (400, 401, 403, 404, 405, 409, 422):
            error = urllib.error.HTTPError("https://git.example", status, SECRET, {},
                                           io.BytesIO(SECRET.encode()))
            with self.subTest(status=status):
                code, output, errors, requests = self.invoke(["merge", "316"], [mr(), error])
                self.assertEqual(code, 1)
                self.assertIn(f"HTTP {status}", errors)
                self.assertNotIn(SECRET, output + errors)
                self.assertNotIn("synthetic-token", output + errors)
                self.assertEqual(len(requests), 2)

    def test_unknown_merge_outcome_requires_show_before_retry(self):
        for response in (urllib.error.URLError(SECRET), TimeoutError(SECRET),
                         http.client.IncompleteRead(SECRET.encode()), http.client.BadStatusLine(SECRET),
                         urllib.error.HTTPError("https://git.example", 503, SECRET, {},
                                                io.BytesIO(SECRET.encode())),
                         SECRET.encode(), b"null", {}, mr(state=SECRET)):
            with self.subTest(response=response):
                code, output, errors, requests = self.invoke(["merge", "316"], [mr(), response])
                self.assertEqual(code, 1)
                self.assertIn("unknown", errors)
                self.assertIn("mr show 316", errors)
                self.assertIn("before any retry", errors)
                self.assertNotIn(SECRET, output + errors)
                self.assertNotIn("synthetic-token", output + errors)
                self.assertEqual(len(requests), 2)

    def test_read_failure_is_safe_without_mutation(self):
        code, output, errors, requests = self.invoke(
            ["show", "316"], [urllib.error.URLError(SECRET)])
        self.assertEqual(code, 1)
        self.assertIn("read failed", errors)
        self.assertNotIn(SECRET, output + errors)
        self.assertEqual([r.method for r in requests], ["GET"])

    def test_invalid_inputs_fail_before_http(self):
        for args in (["show", "0"], ["merge", "-1"], ["show", SECRET],
                     ["merge", "316", "--sha", SECRET],
                     ["show", "316", "--project=../bad"],
                     ["merge", "316", "--project=g/a", "--project=g/b"]):
            with self.subTest(args=args):
                code, output, errors, requests = self.invoke(args, [])
                self.assertNotEqual(code, 0)
                self.assertNotIn(SECRET, output + errors)
                self.assertEqual(requests, [])

    def test_help_exposes_commands_scope_and_cas(self):
        code, output, errors, requests = self.invoke([], [])
        self.assertNotEqual(code, 0)
        self.assertIn("mr show IID", output + errors)
        self.assertIn("mr merge IID", output + errors)
        self.assertIn("--sha", output + errors)
        self.assertIn("--project", output + errors)
        self.assertIn("squash", output + errors)
        self.assertEqual(requests, [])
        with self.assertRaises(SystemExit) as help_text:
            main(["dev"])
        self.assertIn("brood dev mr", str(help_text.exception))
