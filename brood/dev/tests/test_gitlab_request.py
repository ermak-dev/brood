# BUG: Прерванное чтение тела HTTPError обходит нормализацию и раскрывает ответ (#301).
# HYPOTHESIS: except вокруг urlopen не ловит исключения из соседнего except.
# SOLUTION: Общее чтение нормализует обрывы; отказ HTTP не повторяет сырой ответ.
# RESULT: FIXED — обрывы обоих тел дают RuntimeError без секрета и повторной записи.
# STATUS: FIXED
import contextlib
import http.client
import io
import unittest
import urllib.error
from unittest import mock

from brood.dev import gitlab


class RequestBodyTests(unittest.TestCase):
    def test_interrupted_success_and_error_reads_are_uncertain_without_retry(self):
        for status in (200, 500):
            for method in ("PUT", "POST"):
                for failure in (http.client.IncompleteRead(b"response-secret", 2),
                                ConnectionResetError("response-secret"),
                                TimeoutError("response-secret")):
                    with self.subTest(status=status, method=method, failure=type(failure).__name__):
                        response = mock.MagicMock(closed=False)
                        response.read.side_effect = failure
                        response.__enter__.return_value = response
                        with mock.patch.object(gitlab, "auth_header", return_value=("X-Test", "dummy")), \
                                mock.patch.object(gitlab.urllib.request, "urlopen") as urlopen:
                            if status == 200:
                                urlopen.return_value = contextlib.closing(response)
                            else:
                                urlopen.side_effect = urllib.error.HTTPError(
                                    "http://127.0.0.1/test", status, "response-secret", {}, response)
                            with self.assertRaises(RuntimeError) as caught:
                                gitlab.request(method, "http://127.0.0.1/test", "/issues/285", {})
                        self.assertEqual(str(caught.exception),
                                         f"{method} /issues/285: incomplete response; tracker outcome not confirmed")
                        urlopen.assert_called_once()
                        response.read.assert_called_once_with()
                        response.close.assert_called_once()

    def test_complete_error_does_not_echo_secret_response_body(self):
        error = urllib.error.HTTPError("http://127.0.0.1/test", 500, "response-secret", {},
                                       io.BytesIO(b'{"token": "response-secret"}'))
        with mock.patch.object(gitlab, "auth_header", return_value=("X-Test", "dummy")), \
                mock.patch.object(gitlab.urllib.request, "urlopen", side_effect=error) as urlopen:
            with self.assertRaises(RuntimeError) as caught:
                gitlab.request("PUT", "http://127.0.0.1/test", "/issues/285", {})
        self.assertIn("PUT /issues/285 -> HTTP 500", str(caught.exception))
        self.assertNotIn("response-secret", str(caught.exception))
        urlopen.assert_called_once()

    def test_complete_success_returns_body(self):
        response = mock.MagicMock()
        response.__enter__.return_value = response
        response.read.return_value = b'{"ok": true}'
        with mock.patch.object(gitlab, "auth_header", return_value=("X-Test", "dummy")), \
                mock.patch.object(gitlab.urllib.request, "urlopen", return_value=response) as urlopen:
            self.assertEqual(gitlab.request("PUT", "http://127.0.0.1/test", "/issues/285", {}),
                             b'{"ok": true}')
        urlopen.assert_called_once()
