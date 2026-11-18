"""FEATURE: Go deploy доставляет собственный Role для pod своего namespace.

SPEC: Общий historical Role не перезаписывается; чужой объект — отказ до сборки.
Набор глаголов берётся из общего манифеста, второго источника прав нет.
"""
import json
import unittest
from unittest.mock import Mock, patch

from brood.dev.deploy.go_server import Deploy, OWNER, manifests


class GoPodAccessTests(unittest.TestCase):
    def setUp(self):
        dns = patch("socket.getaddrinfo", side_effect=AssertionError("live DNS is forbidden in deploy tests"))
        dns.start()
        self.addCleanup(dns.stop)

    def test_application_manifests_include_owned_read_only_role(self):
        objects = manifests("localhost:5002/brood-go@sha256:" + "a" * 64,
                            {"gvisor": "localhost:5002/worker@sha256:" + "b" * 64})
        roles = [obj for obj in objects if obj["kind"] == "Role"]
        self.assertEqual(len(roles), 1)
        self.assertEqual(roles[0]["metadata"], {
            "name": "brood-go-pod-diagnostics", "namespace": "brood", "labels": OWNER,
        })
        self.assertEqual(roles[0]["rules"], [
            {"apiGroups": [""], "resources": ["pods"], "verbs": ["get", "list", "delete"]},
        ])
        binding = next(obj for obj in objects if obj["kind"] == "RoleBinding")
        self.assertEqual(binding["roleRef"]["name"], "brood-go-pod-diagnostics")
        self.assertEqual(binding["subjects"], [
            {"kind": "ServiceAccount", "name": "django", "namespace": "brood"},
        ])
        self.assertFalse(any(obj["metadata"]["name"] == "django-workerpools" for obj in objects))
        self.assertEqual(objects[-1]["kind"], "NetworkPolicy")

    def test_foreign_diagnostic_role_refused_before_build_or_apply(self):
        cluster = Mock()
        values = {"BROOD_SESSION_SECRET": "x" * 32,
                  "WORKER_POOL_IMAGES": json.dumps({"gvisor": "registry/worker@sha256:" + "b" * 64})}

        def output(*args):
            if "role" in args and "brood-go-pod-diagnostics" in args:
                return 0, '{"metadata":{"labels":{"owner":"foreign"}}}', ""
            if "build" in args or "apply" in args:
                raise AssertionError("mutation started before checking diagnostic Role ownership")
            return 0, "", ""

        cluster.shell.output.side_effect = output
        with patch("brood.dev.deploy.go_server.Infrastructure") as infrastructure:
            infrastructure.return_value._tls.return_value = {"tls.crt": "cert", "tls.key": "key"}
            with self.assertRaisesRegex(RuntimeError, "refusing foreign role/brood-go-pod-diagnostics"):
                Deploy(cluster, get=lambda key: values.get(key, ""), project="/checkout",
                       resolver=Mock(return_value=[(2, 1, 6, "", ("149.154.167.220", 443))])).run()
