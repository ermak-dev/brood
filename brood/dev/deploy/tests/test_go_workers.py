"""Go-сервер наследует только разрешённые образы своего Deployment."""
import json
from types import SimpleNamespace
import unittest
from unittest.mock import Mock

from brood.dev.deploy.go_server import Deploy, manifests


class GoWorkerSettingsTests(unittest.TestCase):
    def test_explicit_lxc_image_is_retained_without_enabling_other_runtimes(self):
        approved = {"gvisor": "localhost:5002/ateworker-gvisor@sha256:" + "a" * 64,
                    "lxc": "localhost:5002/ateworker-lxc@sha256:" + "d" * 64}
        shell = Mock()
        def get(key):
            return json.dumps(approved) if key == "WORKER_POOL_IMAGES" else ""
        self.assertEqual(Deploy(SimpleNamespace(shell=shell), get=get)._worker_images(), approved)
        shell.output.assert_not_called()
        shell.run.assert_not_called()

    def test_missing_worker_settings_are_rejected(self):
        with self.assertRaisesRegex(RuntimeError, "WORKER_POOL_IMAGES"):
            manifests("localhost:5002/brood-go@sha256:" + "a" * 64)

    def test_existing_deployed_image_configuration_is_whitelisted(self):
        approved = {"gvisor": "localhost:5002/ateworker-gvisor@sha256:" + "a" * 64,
                    "microvm": "localhost:5002/ateworker-microvm@sha256:" + "b" * 64}
        deployment = {"metadata": {"labels": {"app.kubernetes.io/managed-by": "brood-go"}},
                      "spec": {"template": {"spec": {"containers": [
                          {"name": "brood", "env": [{"name": "WORKER_POOL_IMAGES",
                                                       "value": json.dumps(approved)}]}]}}}}
        shell = Mock()
        shell.output.return_value = (0, json.dumps(deployment), "")
        result = Deploy(SimpleNamespace(shell=shell), get=lambda _: "")._worker_images()
        self.assertEqual(result, approved)
        self.assertEqual(shell.output.call_count, 1)
        self.assertIn("brood-go", shell.output.call_args.args)
        self.assertNotIn("django", shell.output.call_args.args)

    def test_no_approved_configuration_refuses_without_reading_old_secrets(self):
        shell = Mock()
        shell.output.return_value = (1, "", "not found")
        with self.assertRaisesRegex(RuntimeError, "WORKER_POOL_IMAGES"):
            Deploy(SimpleNamespace(shell=shell), get=lambda _: "")._worker_images()
        self.assertEqual(shell.output.call_count, 1)
        self.assertNotIn("secret", shell.output.call_args.args)
