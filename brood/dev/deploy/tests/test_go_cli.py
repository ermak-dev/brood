"""Явная подготовка Go endpoint не зависит от старого флага."""
import importlib
import unittest
from unittest.mock import patch

command = importlib.import_module("brood.dev.deploy.cli.go")


class GoDeployCliTests(unittest.TestCase):
    def test_preparation_runs_without_optional_flag(self):
        with patch.object(command, "cluster") as cluster, patch.object(command, "Deploy") as deploy:
            deploy.return_value.run.return_value = "registry/brood-go@sha256:digest"
            with patch("builtins.print") as output:
                self.assertEqual(command.main([]), 0)
            deploy.assert_called_once_with(cluster.return_value)
            deploy.return_value.run.assert_called_once_with()
            self.assertIn("Public ingress is unchanged", output.call_args.args[0])
