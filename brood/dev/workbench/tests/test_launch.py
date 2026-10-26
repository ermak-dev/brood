"""Политика сети создаётся до первого запуска нового build-актора."""
import json
import unittest
from unittest import mock

from brood.dev.workbench import launch


class LaunchTests(unittest.TestCase):
    def launcher(self):
        cluster = mock.Mock(kubectl_ate="/substrate/bin/kubectl-ate")
        cluster.shell.output.return_value = (0, "{}", "")
        return launch.Launcher(cluster)

    def test_order_is_template_check_create_policy_resume(self):
        runner = self.launcher()
        events = []

        def output(*args):
            command = args[2:4]
            events.append(command)
            if command == ("get", "actor-template"):
                return 0, json.dumps({"workerSelector": {"matchLabels": {
                    "brood.dev/purpose": "development"}}}), ""
            if command == ("create", "egress-policy"):
                with open(args[args.index("-f") + 1]) as f:
                    policy = json.load(f)
                self.assertEqual(policy["metadata"], {"atespace": "builds", "name": "default"})
            return 0, '{"metadata":{"name":"job"}}', ""

        runner.cluster.shell.output.side_effect = output
        result = runner.start("builds", "brood-build-test-deploy", "job")
        self.assertEqual(result["metadata"]["name"], "job")
        self.assertEqual(events, [("get", "actor-template"), ("create", "actor"),
                                  ("create", "egress-policy"), ("resume", "actor")])
        runner.cluster.check.assert_called_once()

    def test_non_development_template_is_refused(self):
        runner = self.launcher()
        with self.assertRaisesRegex(RuntimeError, "development.*selector"):
            runner.start("builds", "other", "job")
        self.assertEqual(runner.cluster.shell.output.call_count, 1)

    def test_policy_failure_keeps_the_new_actor_suspended(self):
        runner = self.launcher()
        runner.cluster.shell.output.side_effect = [
            (0, '{"workerSelector":{"matchLabels":{"brood.dev/purpose":"development"}}}', ""),
            (0, "{}", ""), (1, "", "policy denied")]
        with self.assertRaisesRegex(RuntimeError, "policy denied.*suspended|suspended.*policy denied"):
            runner.start("builds", "template", "job")
        self.assertFalse(any("resume" in call.args
                             for call in runner.cluster.shell.output.call_args_list))

    def test_shared_identity_warning_does_not_hide_the_actual_refusal(self):
        runner = self.launcher()
        runner.cluster.shell.output.return_value = (1, "", "Warning: no --token-file given\n"
                                                    "Error: no worker has room for the actor\n")
        with self.assertRaisesRegex(RuntimeError, "no worker has room"):
            runner.start("builds", "template", "job")

    def test_ownership_failure_stops_before_calls(self):
        runner = self.launcher()
        runner.cluster.check.side_effect = RuntimeError("foreign installation")
        with self.assertRaisesRegex(RuntimeError, "foreign installation"):
            runner.start("builds", "template", "job")
        runner.cluster.shell.output.assert_not_called()
