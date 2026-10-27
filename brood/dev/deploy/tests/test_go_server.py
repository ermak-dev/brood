"""Параллельная Go-выкладка не заменяет Django и хранит SQLite на PVC."""
import json
import unittest

from brood.dev.deploy import go_server


def render(image):
    return go_server.manifests(image, {"gvisor": "localhost:5002/ateworker-gvisor@sha256:" + "b" * 64})


class GoManifestTests(unittest.TestCase):
    def test_parallel_release_is_isolated_and_persistent(self):
        objects = render("localhost:5002/brood-go@sha256:" + "a" * 64)
        deployment = next(item for item in objects if item["kind"] == "Deployment")
        self.assertEqual(deployment["metadata"]["name"], "brood-go")
        pod = deployment["spec"]["template"]["spec"]
        self.assertEqual(deployment["spec"]["strategy"]["type"], "Recreate")
        self.assertEqual(pod["serviceAccountName"], "django")
        self.assertTrue(any(v.get("persistentVolumeClaim", {}).get("claimName") == "brood-go-data"
                            for v in pod["volumes"]))
        container = pod["containers"][0]
        self.assertTrue(container["securityContext"]["readOnlyRootFilesystem"])
        self.assertEqual(container["securityContext"]["capabilities"]["drop"], ["ALL"])
        self.assertNotIn("envFrom", container)
        self.assertNotIn("django", [o["metadata"]["name"] for o in objects])

    def test_router_configuration_contains_all_required_settings(self):
        deployment = next(item for item in render("localhost:5002/brood-go@sha256:" + "a" * 64)
                          if item["kind"] == "Deployment")
        env = {item["name"]: item.get("value")
               for item in deployment["spec"]["template"]["spec"]["containers"][0]["env"]}
        for key in ("ATENET_ROUTER", "ATENET_CLIENT_CERT_FILE", "ATENET_CLIENT_KEY_FILE",
                    "ATENET_CA_FILE", "ATENET_SERVER_NAME"):
            with self.subTest(key=key):
                self.assertTrue(env.get(key), f"incomplete router configuration: {key}")

    def test_kubernetes_configuration_also_supplies_approved_worker_images(self):
        deployment = next(item for item in render("localhost:5002/brood-go@sha256:" + "a" * 64)
                          if item["kind"] == "Deployment")
        env = {item["name"]: item.get("value")
               for item in deployment["spec"]["template"]["spec"]["containers"][0]["env"]}
        self.assertIn("gvisor", json.loads(env.get("WORKER_POOL_IMAGES", "{}")))

    def test_image_must_be_pinned(self):
        for image in ("localhost:5002/brood-go:latest", "", None):
            with self.subTest(image=image), self.assertRaises(RuntimeError):
                render(image)
