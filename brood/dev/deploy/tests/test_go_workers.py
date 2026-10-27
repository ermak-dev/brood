"""Go-сервер наследует только разрешённые образы, не секреты Django."""
import base64
import json
from types import SimpleNamespace
import unittest
from unittest.mock import Mock

from brood.dev.deploy.go_server import Deploy, manifests


class GoWorkerSettingsTests(unittest.TestCase):
    def test_missing_worker_settings_are_rejected(self):
        with self.assertRaisesRegex(RuntimeError, "WORKER_POOL_IMAGES"):
            manifests("localhost:5002/brood-go@sha256:" + "a" * 64)

    def test_existing_deployed_image_configuration_is_whitelisted(self):
        approved = {"gvisor": "localhost:5002/ateworker-gvisor@sha256:" + "a" * 64,
                    "microvm": "localhost:5002/ateworker-microvm@sha256:" + "b" * 64}
        deployment = {"spec": {"template": {"spec": {"containers": [
            {"name": "django", "envFrom": [{"secretRef": {"name": "brood-env-hash"}}]}]}}}}
        secret = {"data": {"WORKER_POOL_IMAGES": base64.b64encode(json.dumps(approved).encode()).decode(),
                           "DJANGO_SECRET_KEY": base64.b64encode(b"not-for-go").decode()}}
        shell = Mock()
        shell.output.side_effect = [(0, json.dumps(deployment), ""), (0, json.dumps(secret), "")]
        result = Deploy(SimpleNamespace(shell=shell), get=lambda _: "")._worker_images()
        self.assertEqual(result, approved)
        self.assertNotIn("not-for-go", json.dumps(result))
        self.assertEqual(shell.output.call_count, 2)
