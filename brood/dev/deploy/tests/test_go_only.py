"""Единственный серверный runtime — Go; неподготовленный cutover запрещён."""
import importlib
from pathlib import Path
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[4]
DEPLOY = ROOT / "brood/dev/deploy"


class GoOnlyTests(unittest.TestCase):
    def test_legacy_runtime_assets_are_absent(self):
        for name in ("Dockerfile", "Dockerfile.dockerignore", "edge/django.inc", "edge/cliproxy.inc", "server.py",
                     "manifests/django.yaml", "manifests/postgres.yaml",
                     "manifests/cliproxy.yaml", "manifests/cliproxy-config.yaml",
                     "manifests/edge.yaml", "manifests/kustomization.yaml"):
            with self.subTest(name=name):
                self.assertFalse((DEPLOY / name).exists())

    def test_go_does_not_import_old_deployer(self):
        self.assertNotIn("from brood.dev.deploy import server", (DEPLOY / "go_server.py").read_text())

    def test_default_deploy_refuses_missing_settings_before_cluster_access(self):
        command = importlib.import_module("brood.dev.deploy.cli.server")
        with patch.object(command, "cluster") as cluster, \
                patch.object(command.config, "get", return_value=None):
            with self.assertRaisesRegex(RuntimeError, "BROOD_SESSION_SECRET"):
                command.main([])
            cluster.assert_not_called()

    def test_ssh_preflight_does_not_reinstall_running_actor_infrastructure(self):
        script = (ROOT / "deploy/brood-deploy.sh").read_text()
        self.assertNotIn("--if-enabled", script)
        self.assertNotIn("bin/brood substrate install", script)
        self.assertNotIn("bin/brood substrate start", script)
        self.assertLess(script.index("bin/brood deploy server --check"),
                        script.rindex("bin/brood deploy server"))

    def test_no_optional_parallel_mode(self):
        command = importlib.import_module("brood.dev.deploy.cli.go")
        with patch.object(command, "cluster") as cluster:
            with self.assertRaisesRegex(RuntimeError, "takes no arguments"):
                command.main(["--if-enabled"])
            cluster.assert_not_called()
