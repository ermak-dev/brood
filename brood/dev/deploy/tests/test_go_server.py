"""Go-выкладка хранит SQLite на PVC и не меняет другие runtime и данные."""
import json
from pathlib import Path
import unittest
from unittest.mock import Mock, patch

from brood.dev.deploy import go_server


def render(image):
    return go_server.manifests(image, {"gvisor": "localhost:5002/ateworker-gvisor@sha256:" + "b" * 64})


class GoManifestTests(unittest.TestCase):
    def test_go_release_is_isolated_and_persistent(self):
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


class GoDeploymentTests(unittest.TestCase):
    def deploy(self):
        cluster = Mock()
        workers = {"gvisor": "localhost:5002/ateworker-gvisor@sha256:" + "b" * 64}
        values = {"BROOD_SESSION_SECRET": "x" * 32, "WORKER_POOL_IMAGES": json.dumps(workers),
                  "BROOD_RELEASE_SHA": "c" * 40}
        return go_server.Deploy(cluster, get=lambda key: values.get(key, ""), project="/checkout")

    def test_build_and_apply_only_go_without_deleting_data(self):
        deploy = self.deploy()
        events = []
        digest = "sha256:" + "a" * 64

        def output(*args):
            self.assertNotIn("delete", args)
            self.assertNotIn("scale", args)
            if "get" in args:
                events.append("ownership")
                return 0, "", ""
            if "inspect" in args:
                events.append("registry")
                return 0, '{"IPAddress":"172.18.0.3"}', ""
            if "build" in args:
                events.append("build")
                self.assertEqual(args[args.index("-f") + 1], "/checkout/deploy/Dockerfile.go")
                self.assertIn("--build-arg", args)
                self.assertIn("VERSION=" + "c" * 40, args)
                Path(args[args.index("--metadata-file") + 1]).write_text(
                    json.dumps({"containerimage.digest": digest}))
                return 0, "", ""
            if "apply" in args:
                events.append("apply")
                path = Path(args[args.index("-f") + 1])
                self.assertEqual(path.stat().st_mode & 0o777, 0o600)
                objects = json.loads(path.read_text())["items"]
                deployments = [o for o in objects if o["kind"] == "Deployment"]
                self.assertEqual([o["metadata"]["name"] for o in deployments], ["brood-go"])
                pod = deployments[0]["spec"]["template"]["spec"]
                self.assertEqual(pod["containers"][0]["image"], "localhost:5002/brood-go@" + digest)
                self.assertNotIn("initContainers", pod)
                self.assertFalse(any(o["kind"] in ("WorkerPool", "StatefulSet") for o in objects))
                return 0, "", ""
            self.assertIn("rollout", args)
            events.append("rollout")
            return 0, "", ""

        deploy.cluster.shell.output.side_effect = output
        with patch.object(go_server, "Infrastructure") as infrastructure:
            infrastructure.return_value._tls.return_value = {"tls.crt": "cert", "tls.key": "key"}
            infrastructure.return_value._api_egress.side_effect = lambda: events.append("discovery") or []
            self.assertEqual(deploy.run(), "localhost:5002/brood-go@" + digest)
        self.assertLess(events.index("ownership"), events.index("discovery"))
        self.assertLess(events.index("discovery"), events.index("registry"))
        self.assertLess(events.index("registry"), events.index("build"))
        self.assertLess(events.index("build"), events.index("apply"))
        self.assertLess(events.index("apply"), events.index("rollout"))
        deploy.cluster.shell.run.assert_not_called()

    def test_malformed_ownership_response_refuses_before_build(self):
        for response in ("not json", "[]", '{"metadata":null}', '{"metadata":{"labels":[]}}'):
            with self.subTest(response=response):
                deploy = self.deploy()
                deploy.cluster.shell.output.return_value = (0, response, "")
                with patch.object(go_server, "Infrastructure"):
                    with self.assertRaisesRegex(RuntimeError, "cannot check ownership"):
                        deploy.run()
                deploy.cluster.shell.output.assert_called_once()
