"""Публикация использует внутренний registry, без нового внешнего listener."""
import unittest

from brood.dev.deploy import go_server


class RegistryAccessTests(unittest.TestCase):
    # FEATURE: только Go BFF получает доступ к проверенному registry в kind network.
    # SPEC: точный IP/порт, приватный URL и worker image host, без внешнего ingress.
    def test_internal_registry_has_exact_egress_and_server_settings(self):
        endpoint = go_server.registry_endpoint('{"IPAddress":"172.18.0.3"}')
        self.assertEqual(endpoint["url"], "http://172.18.0.3:5000")
        objects = go_server.manifests("localhost:5002/brood-go@sha256:" + "a" * 64,
            {"gvisor": "localhost:5002/worker@sha256:" + "b" * 64}, registry=endpoint)
        deployment = next(obj for obj in objects if obj["kind"] == "Deployment")
        env = {item["name"]: item.get("value")
               for item in deployment["spec"]["template"]["spec"]["containers"][0]["env"]}
        self.assertEqual(env["BROOD_REGISTRY_URL"], "http://172.18.0.3:5000")
        self.assertEqual(env["BROOD_REGISTRY_IMAGE_HOST"], "localhost:5002")
        policy = next(obj for obj in objects if obj["kind"] == "NetworkPolicy")
        self.assertIn({"to": [{"ipBlock": {"cidr": "172.18.0.3/32"}}],
                       "ports": [{"port": 5000, "protocol": "TCP"}]}, policy["spec"]["egress"])
        self.assertFalse(any(obj["kind"] == "Service" and obj["metadata"]["name"] == "registry"
                             for obj in objects))

    def test_invalid_or_public_registry_endpoint_refuses(self):
        for text in ("null", "not-json", "{}", '{"IPAddress":"127.0.0.1"}',
                     '{"IPAddress":"8.8.8.8"}', '{"IPAddress":"0.0.0.0"}'):
            with self.subTest(text=text), self.assertRaisesRegex(RuntimeError, "registry"):
                go_server.registry_endpoint(text)
