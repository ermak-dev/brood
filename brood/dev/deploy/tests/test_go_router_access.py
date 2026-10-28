"""BUG: Actor proxy получает 502, когда egress разрешён только к Service-порту.

HYPOTHESIS: После DNAT router Service:443 ведёт на pod:8443, запрещённый политикой.
SOLUTION: Разрешён TCP:8443 только к app=atenet-router в namespace ate-system.
"""
import unittest

from brood.dev.deploy.go_server import manifests


class GoRouterAccessTests(unittest.TestCase):
    def test_router_backend_port_is_allowed_only_to_router_pods(self):
        objects = manifests("localhost:5002/brood-go@sha256:" + "a" * 64,
                            {"gvisor": "localhost:5002/worker@sha256:" + "b" * 64})
        policy = next(obj for obj in objects if obj["kind"] == "NetworkPolicy")
        router_rules = [rule for rule in policy["spec"]["egress"]
                        if any(port["port"] == 8443 for port in rule["ports"])]
        self.assertEqual(router_rules, [{
            "to": [{
                "namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "ate-system"}},
                "podSelector": {"matchLabels": {"app": "atenet-router"}},
            }],
            "ports": [{"port": 8443, "protocol": "TCP"}],
        }])
        self.assertIn({"to": [{"namespaceSelector": {
            "matchLabels": {"kubernetes.io/metadata.name": "ate-system"}}}],
            "ports": [{"port": 443}]}, policy["spec"]["egress"])
