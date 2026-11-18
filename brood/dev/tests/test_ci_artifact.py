# FEATURE: Скачать один файл артефакта GitLab по точному ID джобы и пути (#313).
# SPEC: Только прямой GET, ограниченные порции, проверка SHA-256 и публикация без перезаписи.
# IMPLEMENTATION: Заменяем транспорт urllib, сохраняя обработку HTTP и работу с файлами.
import contextlib
import hashlib
import io
import tempfile
import unittest
import urllib.request
from email.message import Message
from pathlib import Path
from unittest import mock
from urllib.response import addinfourl

from brood.common import cli
from brood.dev import gitlab


class ArtifactTests(unittest.TestCase):
    def setUp(self):
        directory = self.enterContext(tempfile.TemporaryDirectory())
        self.directory = Path(directory)
        self.output = self.directory / "download.bin"
        self.settings = {"GITLAB_TOKEN": "synthetic-token"}
        self.enterContext(mock.patch.object(gitlab.config, "get", side_effect=self.settings.get))
        self.enterContext(mock.patch.object(gitlab, "origin",
                                            return_value="git@gitlab.invalid:group/sub/project.git"))
        self.enterContext(mock.patch.object(urllib.request, "getproxies", return_value={}))
        self.https = self.enterContext(mock.patch.object(urllib.request.HTTPSHandler, "https_open",
                                                         autospec=True))
        self.http = self.enterContext(mock.patch.object(urllib.request.HTTPHandler, "http_open",
                                                        autospec=True))

    def respond(self, body, status=200, **headers):
        message = Message()
        for name, value in headers.items():
            message[name.replace("_", "-")] = value
        response = addinfourl(io.BytesIO(body), message, "https://gitlab.invalid/", status)
        response.msg = "synthetic-response-secret"
        self.https.return_value = response
        return response

    def test_cli_streams_exact_file_and_checks_digest(self):
        body = bytes(range(256)) * 1024
        response = self.respond(body, Content_Length=str(len(body)))
        stdout = io.StringIO()
        with mock.patch.object(response, "read", wraps=response.read) as read, \
                contextlib.redirect_stdout(stdout):
            self.assertEqual(cli.main([
                "dev", "ci", "artifact", "42", "dir/é #?%2F.bin", "--out", str(self.output),
                "--sha256", hashlib.sha256(body).hexdigest().upper(),
            ]), 0)
        self.assertGreater(read.call_count, 1)
        for call in read.call_args_list:
            (size,) = call.args
            self.assertGreater(size, 0)
            self.assertLessEqual(size, 64 * 1024)
        request = self.https.call_args.args[1]
        self.assertEqual(self.https.call_count, 1)
        self.assertEqual(request.get_method(), "GET")
        self.assertEqual(request.full_url,
                         "https://gitlab.invalid/api/v4/projects/group%2Fsub%2Fproject/"
                         "jobs/42/artifacts/dir/%C3%A9%20%23%3F%252F.bin")
        self.assertEqual(request.get_header("Private-token"), "synthetic-token")
        self.assertEqual(stdout.getvalue(), "Artifact downloaded.\n")
        self.assertEqual(self.output.read_bytes(), body)
        self.assertEqual(list(self.directory.iterdir()), [self.output])
        self.assertTrue(response.closed)

    def test_config_overrides_and_empty_file_without_digest(self):
        self.settings.update(GITLAB_SITE="other.invalid:8443", GITLAB_PROJECT="other/project")
        self.respond(b"", Content_Length="0")
        gitlab.download_artifact(42, "file", self.output)
        self.assertEqual(self.https.call_args.args[1].full_url,
                         "https://other.invalid:8443/api/v4/projects/other%2Fproject/"
                         "jobs/42/artifacts/file")
        self.assertEqual(self.output.read_bytes(), b"")

    def test_rejects_invalid_input_before_request(self):
        cases = [
            ((job, "file", self.output, None), "job ID")
            for job in ("", "0", "-1", "1/2", "1?x=2", "４２", True, None)
        ] + [
            ((42, path, self.output, None), "artifact path")
            for path in ("", "/file", "a/", "a//b", "./b", "a/../b", "a\\b", "a\x00b", "a\nb")
        ] + [
            ((42, "file", output, None), "output")
            for output in ("", None, str(self.output) + "/", "bad\x00name",
                           self.directory / "missing" / "file",
                           self.directory / "missing" / ".." / "file")
        ] + [
            ((42, "file", self.output, digest), "SHA-256")
            for digest in ("", "x" * 64, "0" * 63, "0" * 64 + "\n")
        ]
        for args, message in cases:
            with self.subTest(args=args), self.assertRaisesRegex(RuntimeError, message):
                gitlab.download_artifact(*args)
        self.https.assert_not_called()
        self.assertEqual(list(self.directory.iterdir()), [])

    def test_existing_file_directory_and_dangling_symlink_are_never_replaced(self):
        existing = self.directory / "existing"
        existing.write_bytes(b"original")
        self.output.symlink_to(self.directory / "missing")
        for output in (existing, self.directory, self.output):
            with self.subTest(output=output), self.assertRaisesRegex(RuntimeError, "output already exists"):
                gitlab.download_artifact(42, "file", output)
        self.https.assert_not_called()
        self.assertEqual(existing.read_bytes(), b"original")
        self.assertTrue(self.output.is_symlink())
        self.assertFalse(self.output.exists())
        self.assertCountEqual(self.directory.iterdir(), [existing, self.output])

    def test_competing_destination_is_not_clobbered(self):
        response = self.respond(b"artifact", Content_Length="8")
        read = response.read

        def competing_write(size):
            if not self.output.exists():
                (scratch,) = self.directory.iterdir()
                self.assertNotEqual(scratch, self.output)
                self.output.write_bytes(b"winner")
            return read(size)

        with mock.patch.object(response, "read", side_effect=competing_write), \
                self.assertRaisesRegex(RuntimeError, "output already exists"):
            gitlab.download_artifact(42, "file", self.output)
        self.assertEqual(self.output.read_bytes(), b"winner")
        self.assertEqual(list(self.directory.iterdir()), [self.output])

    def test_publish_failure_cleans_scratch_and_suppresses_os_error_details(self):
        self.respond(b"artifact", Content_Length="8")
        with mock.patch.object(gitlab.os, "link", side_effect=OSError("filesystem-secret")), \
                self.assertRaisesRegex(RuntimeError, "could not write artifact output") as caught:
            gitlab.download_artifact(42, "file", self.output)
        self.assertNotIn("secret", str(caught.exception))
        self.assertEqual(list(self.directory.iterdir()), [])

    def test_refuses_redirects_without_reading_body_or_forwarding_credentials(self):
        for status, location in ((301, "/same-origin"), (302, "https://cdn.invalid/signed-secret"),
                                 (303, "http://cdn.invalid/signed-secret"), (304, "/cache"),
                                 (307, "//cdn.invalid/signed-secret"), (308, None),
                                 (302, "https://[malformed-signed-secret")):
            with self.subTest(status=status):
                self.https.reset_mock()
                response = self.respond(b"response-secret", status, **({"Location": location} if location else {}))
                with mock.patch.object(response, "read") as read, \
                        self.assertRaisesRegex(RuntimeError, "redirect refused") as caught:
                    gitlab.download_artifact(42, "file", self.output)
                self.assertEqual(self.https.call_count, 1)
                self.http.assert_not_called()
                read.assert_not_called()
                self.assertNotIn("secret", str(caught.exception))
                self.assertTrue(response.closed)
                self.assertEqual(list(self.directory.iterdir()), [])

    def test_refuses_http_errors_and_partial_content_without_exposing_response(self):
        for status in (206, 401, 403, 404, 500):
            with self.subTest(status=status):
                response = self.respond(b"response-secret", status)
                with mock.patch.object(response, "read") as read, \
                        self.assertRaisesRegex(RuntimeError, f"HTTP {status}") as caught:
                    gitlab.download_artifact(42, "file", self.output)
                read.assert_not_called()
                self.assertNotIn("secret", str(caught.exception))
                self.assertTrue(response.closed)
                self.assertEqual(list(self.directory.iterdir()), [])

    def test_incomplete_or_interrupted_body_is_not_published(self):
        for failure in (None, ConnectionResetError("transport-secret")):
            with self.subTest(failure=failure):
                response = self.respond(b"partial", Content_Length="12")
                read = response.read
                side_effect = [b"partial", failure] if failure else read
                with mock.patch.object(response, "read", side_effect=side_effect), \
                        self.assertRaisesRegex(RuntimeError, "incomplete response") as caught:
                    gitlab.download_artifact(42, "file", self.output)
                self.assertNotIn("secret", str(caught.exception))
                self.assertTrue(response.closed)
                self.assertEqual(list(self.directory.iterdir()), [])

    def test_requires_digest_when_response_has_no_length(self):
        response = self.respond(b"artifact")
        with mock.patch.object(response, "read") as read, \
                self.assertRaisesRegex(RuntimeError, "provide --sha256"):
            gitlab.download_artifact(42, "file", self.output)
        read.assert_not_called()
        self.assertEqual(list(self.directory.iterdir()), [])

    def test_digest_mismatch_does_not_publish_or_print_digest(self):
        self.respond(b"artifact")
        digest = "0" * 64
        with self.assertRaisesRegex(RuntimeError, "SHA-256 mismatch") as caught:
            gitlab.download_artifact(42, "file", self.output, digest)
        self.assertNotIn(digest, str(caught.exception))
        self.assertNotIn(hashlib.sha256(b"artifact").hexdigest(), str(caught.exception))
        self.assertEqual(list(self.directory.iterdir()), [])
