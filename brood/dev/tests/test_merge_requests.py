import contextlib
import http.client
import io
import unittest
from unittest import mock

from brood.dev import gitlab, merge_requests


SHA = "a" * 40
OTHER = "b" * 40
SECRET = "credential-sentinel"


def mr(**changes):
    return {"iid": 316, "state": "opened", "sha": SHA,
            "merge_commit_sha": None, **changes}


class MergeRequestsTests(unittest.TestCase):
    def test_show_is_get_only_and_returns_data_silently(self):
        data = mr(description=SECRET)
        with mock.patch.object(gitlab, "call", return_value=data) as call, \
                contextlib.redirect_stdout(io.StringIO()) as output:
            self.assertEqual(merge_requests.show(316), data)
        call.assert_called_once_with("GET", "/merge_requests/316")
        self.assertEqual(output.getvalue(), "")

    def test_merge_reads_head_and_sends_exact_cas_without_scheduling(self):
        for expected in (None, SHA):
            with self.subTest(expected=expected), mock.patch.object(
                    gitlab, "call", side_effect=[mr(), mr(state="merged")]) as call:
                result = merge_requests.merge("316", sha=expected)
                self.assertEqual(result["state"], "merged")
                self.assertIsNone(result["merge_commit_sha"])
                self.assertEqual(call.call_args_list, [
                    mock.call("GET", "/merge_requests/316"),
                    mock.call("PUT", "/merge_requests/316/merge",
                              {"squash": True, "auto_merge": False, "sha": SHA})])

    def test_opened_response_remains_opened_not_completion(self):
        with mock.patch.object(gitlab, "call", side_effect=[mr(), mr(auto_merge=True)]):
            self.assertEqual(merge_requests.merge(316)["state"], "opened")

    def test_bad_local_inputs_never_call_transport_or_echo_input(self):
        for iid in (0, -1, True, None, "", "01", "３１６", "316/merge", SECRET):
            for function in (merge_requests.show, merge_requests.merge):
                with self.subTest(iid=iid, function=function), \
                        mock.patch.object(gitlab, "call") as call, \
                        self.assertRaises(RuntimeError) as error:
                    function(iid)
                call.assert_not_called()
                self.assertNotIn(SECRET, str(error.exception))
        for sha in ("", "abc", SECRET, 123, "a" * 41):
            with self.subTest(sha=sha), mock.patch.object(gitlab, "call") as call, \
                    self.assertRaisesRegex(RuntimeError, "SHA"):
                merge_requests.merge(316, sha=sha)
            call.assert_not_called()

    def test_oversized_iid_is_rejected_before_transport(self):
        for value in (str(2**63), "9" * 5000):
            with mock.patch.object(gitlab, "call") as call:
                with self.assertRaisesRegex(RuntimeError, "invalid MR IID"):
                    merge_requests.show(value)
                call.assert_not_called()

    def test_optional_sha_drift_prevents_put(self):
        with mock.patch.object(gitlab, "call", return_value=mr()) as call, \
                self.assertRaisesRegex(RuntimeError, "HEAD.*mismatch"):
            merge_requests.merge(316, sha=OTHER)
        call.assert_called_once_with("GET", "/merge_requests/316")

    def test_merged_is_read_only_and_closed_is_refused(self):
        with mock.patch.object(gitlab, "call", return_value=mr(state="merged")) as call:
            self.assertEqual(merge_requests.merge(316)["state"], "merged")
        self.assertEqual(call.call_count, 1)
        with mock.patch.object(gitlab, "call", return_value=mr(state="closed")) as call, \
                self.assertRaisesRegex(RuntimeError, "closed"):
            merge_requests.merge(316)
        self.assertEqual(call.call_count, 1)

    def test_documented_http_refusals_are_safe_and_never_retried(self):
        for status, reason in ((400, "SHA"), (401, "rights"), (403, "rights"),
                               (404, "not found"), (405, "eligible"),
                               (409, "HEAD"), (422, "execution failed")):
            error = RuntimeError(f"PUT /merge_requests/316/merge -> HTTP {status}: {SECRET}")
            with self.subTest(status=status), mock.patch.object(
                    gitlab, "call", side_effect=[mr(), error]) as call, \
                    self.assertRaises(RuntimeError) as caught:
                merge_requests.merge(316)
            self.assertIn(f"HTTP {status}", str(caught.exception))
            self.assertIn(reason, str(caught.exception))
            self.assertNotIn(SECRET, str(caught.exception))
            self.assertEqual(call.call_count, 2)

    def test_lost_or_unexpected_put_response_is_unknown_without_retry(self):
        for response in (RuntimeError(SECRET), OSError(SECRET), ValueError(SECRET),
                         http.client.BadStatusLine(SECRET),
                         RuntimeError(f"PUT /merge_requests/316/merge -> HTTP 503: {SECRET}"),
                         None, [], {}, mr(iid=317), mr(sha=OTHER),
                         mr(state=SECRET), mr(merge_commit_sha=SECRET)):
            with self.subTest(response=response), mock.patch.object(
                    gitlab, "call", side_effect=[mr(), response]) as call, \
                    self.assertRaises(RuntimeError) as error:
                merge_requests.merge(316)
            message = str(error.exception)
            self.assertIn("unknown", message)
            self.assertIn("mr show 316", message)
            self.assertIn("retry", message)
            self.assertNotIn(SECRET, message)
            self.assertEqual(call.call_count, 2)

    def test_bad_get_cannot_reach_put_and_errors_are_sanitized(self):
        for response in (RuntimeError(SECRET), ValueError(SECRET), None, [], {},
                         mr(iid=True), mr(iid=317), mr(sha=SECRET), mr(state=SECRET)):
            with self.subTest(response=response), mock.patch.object(
                    gitlab, "call", side_effect=[response]) as call, \
                    self.assertRaises(RuntimeError) as error:
                merge_requests.merge(316)
            self.assertNotIn(SECRET, str(error.exception))
            self.assertNotIn("unknown", str(error.exception))
            self.assertEqual(call.call_count, 1)
