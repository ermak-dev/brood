"""Опциональная Go-выкладка не затрагивает установку без флага."""
import importlib
import unittest
from unittest.mock import patch

command = importlib.import_module("brood.dev.deploy.cli.go")


class GoDeployCliTests(unittest.TestCase):
    def test_disabled_parallel_rollout_never_constructs_cluster(self):
        with patch.object(command.config, "get", return_value=""), patch.object(command, "cluster") as cluster:
            self.assertEqual(command.main(["--if-enabled"]), 0)
            cluster.assert_not_called()
