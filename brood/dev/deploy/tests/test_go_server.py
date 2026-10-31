"""Go-выкладка хранит SQLite на PVC и не меняет другие runtime и данные."""
import base64
import hashlib
import json
import socket
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

    def test_https_origins_reject_malformed_authorities_without_resolution(self):
        for origin in ("https://[2001:db8::1]junk", "https://brood.invalid\u007f",
                       "https://brood.invalid:", "https://brood.invalid:0", "https://brood.invalid/?"):
            with self.subTest(origin=origin), self.assertRaisesRegex(RuntimeError, "BROOD_PUBLIC_ORIGIN"):
                go_server.https_origin(origin, "BROOD_PUBLIC_ORIGIN")

    def test_runtime_pool_top_level_fields_and_per_endpoint_ports(self):
        for field in ("template", "templateDigest", "capacity"):
            self.assertEqual(go_server.proxmox_runtime_endpoints(json.dumps({"lab": {
                field: None, "endpoint": "https://pve.invalid:8006"}})), {"https://pve.invalid:8006"})
        self.assertEqual(go_server.proxmox_runtime_endpoints('{"lab":{"endpoint":"https://unused.invalid"}}'), set())
        resolver = Mock(side_effect=lambda host, port, **kwargs: [
            (socket.AF_INET, socket.SOCK_STREAM, 6, "", ("10.23.4.5" if port == 443 else "10.23.4.6", port))])
        self.assertEqual(go_server.proxmox_egress({"https://pve.invalid", "https://other.invalid:8006"}, resolver), [
            {"to": [{"ipBlock": {"cidr": "10.23.4.5/32"}}], "ports": [{"port": 443, "protocol": "TCP"}]},
            {"to": [{"ipBlock": {"cidr": "10.23.4.6/32"}}], "ports": [{"port": 8006, "protocol": "TCP"}]}])
        deploy = go_server.Deploy(Mock(), resolver=resolver)
        self.assertIs(deploy.resolver, resolver)
        for endpoint in ("http://pve.invalid", "https://pve.invalid:0", "https://pve.invalid:65536",
                         "https://token@pve.invalid", "https://pve.invalid/api2", "https://pve.invalid?x"):
            resolver.reset_mock()
            with self.subTest(endpoint=endpoint), self.assertRaisesRegex(RuntimeError, "BROOD_PROXMOX_POOLS"):
                go_server.proxmox_egress({endpoint}, resolver)
            resolver.assert_not_called()

    def test_mapped_ipv4_egress_uses_kubernetes_valid_ipv4_cidr(self):
        resolver = Mock(return_value=[
            (socket.AF_INET6, socket.SOCK_STREAM, 6, "", ("::ffff:10.23.4.5", 8006, 0, 0)),
            (socket.AF_INET, socket.SOCK_STREAM, 6, "", ("10.23.4.5", 8006)),
        ])
        self.assertEqual(go_server.proxmox_egress({"https://[::ffff:10.23.4.5]:8006"}, resolver), [
            {"to": [{"ipBlock": {"cidr": "10.23.4.5/32"}}],
             "ports": [{"port": 8006, "protocol": "TCP"}]}])

    def test_image_must_be_pinned(self):
        for image in ("localhost:5002/brood-go:latest", "", None):
            with self.subTest(image=image), self.assertRaises(RuntimeError):
                render(image)


class GoDeploymentTests(unittest.TestCase):
    def deploy(self, settings=None):
        cluster = Mock()
        workers = {"gvisor": "localhost:5002/ateworker-gvisor@sha256:" + "b" * 64}
        values = {"BROOD_SESSION_SECRET": "x" * 32, "WORKER_POOL_IMAGES": json.dumps(workers),
                  "BROOD_RELEASE_SHA": "c" * 40}
        values.update(settings or {})
        return go_server.Deploy(cluster, get=lambda key: values.get(key, ""), project="/checkout")

    def capture_release(self, settings):
        deploy = self.deploy(settings)
        applied = []

        def output(*args):
            for key in ("BROOD_SESSION_SECRET", *go_server.PROXMOX_SETTINGS,
                        "BROOD_PUBLIC_ORIGIN", "BROOD_PROXMOX_RELAY_SECRET",
                        "CLIPROXY_URL", "CLIPROXY_MANAGEMENT_KEY"):
                value = settings.get(key)
                if value:
                    self.assertNotIn(value, args)
            if "build" in args:
                Path(args[args.index("--metadata-file") + 1]).write_text(
                    json.dumps({"containerimage.digest": "sha256:" + "a" * 64}))
            if "apply" in args:
                applied.extend(json.loads(Path(args[args.index("-f") + 1]).read_text())["items"])
            return 0, "", ""

        deploy.cluster.shell.output.side_effect = output
        with patch.object(go_server, "Infrastructure") as infrastructure, \
                patch.object(deploy, "_registry_access", return_value=None), \
                patch("builtins.print") as printed:
            infrastructure.return_value._tls.return_value = {"tls.crt": "cert", "tls.key": "key"}
            infrastructure.return_value._api_egress.return_value = []
            deploy.run()
            printed.assert_not_called()
        return applied

    # BUG: stringData не удаляет сохранённые data-ключи после отключения Proxmox.
    # HYPOTHESIS: apply должен владеть настоящими data-ключами, не входным stringData.
    def test_server_secret_owns_encoded_data_and_removes_only_previous_keys(self):
        settings = {
            "BROOD_PROXMOX_POOLS": '{ "lab": {"caPEM": "line1\\nline2\\n", "name": "тест"} }\n',
            "BROOD_PROXMOX_TOKENS": '{ "lab-token": "synthetic-token=\\n" }\n',
        }
        previous = self.capture_release(settings)
        current = self.capture_release({})
        previous_secret, current_secret = [
            next(o for o in objects if o["metadata"]["name"] == "brood-go-env")
            for objects in (previous, current)]
        for secret, expected in ((previous_secret, {"BROOD_SESSION_SECRET": "x" * 32, **settings}),
                                 (current_secret, {"BROOD_SESSION_SECRET": "x" * 32})):
            self.assertNotIn("stringData", secret)
            self.assertEqual(secret.get("data"), {
                key: base64.b64encode(value.encode("utf-8")).decode("ascii")
                for key, value in expected.items()})
            self.assertEqual({key: base64.b64decode(value).decode("utf-8")
                              for key, value in secret["data"].items()}, expected)
        # Модель удаления ключей map, которыми владеет last-applied; не эмуляция API.
        stored = {**previous_secret["data"], "unrelated": "a2VlcA=="}
        for key in previous_secret["data"].keys() - current_secret["data"].keys():
            stored.pop(key)
        stored.update(current_secret["data"])
        self.assertEqual(stored, {**current_secret["data"], "unrelated": "a2VlcA=="})
        self.assertTrue(set(go_server.PROXMOX_SETTINGS).isdisjoint(stored))

    # BUG: Обновление Secret при том же digest не перезапускает сервер с новыми env.
    # HYPOTHESIS: Хеш всего env Secret в pod template меняет шаблон без раскрытия значений.
    def test_server_config_checksum_rolls_same_image_on_every_secret_change(self):
        pools = {"lab": {"endpoint": "https://pve.invalid:8006", "caPEM": "synthetic-ca"}}
        settings = {
            "BROOD_SESSION_SECRET": "synthetic-session-" * 3,
            "BROOD_PROXMOX_POOLS": json.dumps(pools),
            "BROOD_PROXMOX_TOKENS": json.dumps({"lab": "synthetic-token"}),
            "CLIPROXY_URL": "http://synthetic-proxy.invalid:8317",
            "CLIPROXY_MANAGEMENT_KEY": "synthetic-proxy-key",
        }

        def checksum(values):
            objects = self.capture_release(values)
            deployment = next(o for o in objects if o["kind"] == "Deployment")
            template = deployment["spec"]["template"]
            self.assertEqual(template["spec"]["containers"][0]["image"],
                             "localhost:5002/brood-go@sha256:" + "a" * 64)
            checksum = template["metadata"].get("annotations", {}).get("brood.dev/server-env-sha256")
            self.assertIsNotNone(checksum, "server env changes need a pod-template checksum")
            data = next(o for o in objects if o["metadata"]["name"] == "brood-go-env")["data"]
            self.assertEqual(checksum, hashlib.sha256(
                json.dumps(data, sort_keys=True, separators=(",", ":")).encode("utf-8")).hexdigest())
            env = {entry["name"]: entry for entry in template["spec"]["containers"][0]["env"]}
            for key in data:
                self.assertEqual(env[key], {"name": key, "valueFrom": {
                    "secretKeyRef": {"name": "brood-go-env", "key": key}}})
            for value in (*values.values(), *data.values()):
                self.assertNotIn(value, json.dumps(deployment))
            return checksum

        original = checksum(settings)
        self.assertEqual(checksum(dict(reversed(list(settings.items())))), original)
        variants = {
            "token": {**settings, "BROOD_PROXMOX_TOKENS": '{"lab":"rotated-token"}'},
            "endpoint": {**settings, "BROOD_PROXMOX_POOLS": json.dumps({"lab": {
                **pools["lab"], "endpoint": "https://changed.invalid:8006"}})},
            "ca": {**settings, "BROOD_PROXMOX_POOLS": json.dumps({"lab": {
                **pools["lab"], "caPEM": "rotated-ca"}})},
            "remove": {key: value for key, value in settings.items() if key not in go_server.PROXMOX_SETTINGS},
            "session": {**settings, "BROOD_SESSION_SECRET": "rotated-session-" * 3},
            "proxy_url": {**settings, "CLIPROXY_URL": "http://changed-proxy.invalid:8317"},
            "proxy_key": {**settings, "CLIPROXY_MANAGEMENT_KEY": "rotated-proxy-key"},
            "proxy_remove": {key: value for key, value in settings.items() if not key.startswith("CLIPROXY_")},
        }
        for name, values in variants.items():
            with self.subTest(change=name):
                self.assertNotEqual(checksum(values), original)

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
                self.assertNotIn("BROOD_PROXMOX_", json.dumps(objects))
                secret = next(o for o in objects if o["metadata"]["name"] == "brood-go-env")
                self.assertNotIn("stringData", secret)
                self.assertEqual(secret["data"], {
                    "BROOD_SESSION_SECRET": base64.b64encode(("x" * 32).encode()).decode("ascii")})
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

    # FEATURE: Настройки Proxmox передаются только серверу через Secret.
    # SPEC: Явный список ключей, парность до сборки и записи, без новых ресурсов и egress.
    def test_proxmox_settings_reach_only_server_secret(self):
        settings = {
            "BROOD_PROXMOX_POOLS": json.dumps({"lab": {
                "endpoint": "https://pve.invalid:8006", "tokenID": "test@pve!brood",
                "node": "node", "resourcePool": "brood", "storage": "local",
                "network": "vmbr0", "credentialRef": "lab-token", "caPEM": "test-ca-sentinel"}}),
            "BROOD_PROXMOX_TOKENS": json.dumps({"lab-token": "proxmox-token-sentinel"}),
        }
        deploy = self.deploy({**settings, "UNRELATED_SECRET": "unrelated-sentinel",
                              "BROOD_PROXMOX_OTHER": "other-sentinel"})
        applied = []

        def output(*args):
            for value in settings.values():
                self.assertNotIn(value, args)
            self.assertNotIn("delete", args)
            self.assertNotIn("scale", args)
            if "build" in args:
                Path(args[args.index("--metadata-file") + 1]).write_text(
                    json.dumps({"containerimage.digest": "sha256:" + "a" * 64}))
            if "apply" in args:
                path = Path(args[args.index("-f") + 1])
                self.assertEqual(path.stat().st_mode & 0o777, 0o600)
                applied.extend(json.loads(path.read_text())["items"])
            return 0, "", ""

        deploy.cluster.shell.output.side_effect = output
        with patch.object(go_server, "Infrastructure") as infrastructure, \
                patch.object(deploy, "_registry_access", return_value=None):
            infrastructure.return_value._tls.return_value = {"tls.crt": "cert", "tls.key": "key"}
            infrastructure.return_value._api_egress.return_value = []
            deploy.run()
        secret = next(o for o in applied if o["metadata"]["name"] == "brood-go-env")
        self.assertNotIn("stringData", secret)
        self.assertEqual(secret["data"], {
            key: base64.b64encode(value.encode("utf-8")).decode("ascii")
            for key, value in {"BROOD_SESSION_SECRET": "x" * 32, **settings}.items()})
        deployment = next(o for o in applied if o["kind"] == "Deployment")
        env = {e["name"]: e for e in deployment["spec"]["template"]["spec"]["containers"][0]["env"]}
        for key in settings:
            self.assertEqual(env.get(key), {"name": key, "valueFrom": {
                "secretKeyRef": {"name": "brood-go-env", "key": key}}})
        baseline = render("localhost:5002/brood-go@sha256:" + "a" * 64)
        self.assertEqual([(o["kind"], o["metadata"]["name"]) for o in applied if o["kind"] != "Secret"],
                         [(o["kind"], o["metadata"]["name"]) for o in baseline])
        for obj in applied:
            self.assertNotIn("unrelated-sentinel", json.dumps(obj))
            self.assertNotIn("other-sentinel", json.dumps(obj))
            if obj is not secret:
                self.assertNotIn("proxmox-token-sentinel", json.dumps(obj))
                self.assertNotIn("test-ca-sentinel", json.dumps(obj))
                for value in settings.values():
                    self.assertNotIn(value, json.dumps(obj))
            if obj["kind"] not in ("Secret", "Deployment"):
                for key in settings:
                    self.assertNotIn(key, json.dumps(obj))
            if obj["kind"] == "NetworkPolicy":
                self.assertEqual(obj, next(o for o in baseline if o["kind"] == "NetworkPolicy"))
        self.assertFalse(any(o["kind"] in ("Actor", "WorkerPool", "StatefulSet") for o in applied))

    def test_partial_proxmox_settings_refuse_before_native_calls(self):
        for missing, present in (("BROOD_PROXMOX_POOLS", "BROOD_PROXMOX_TOKENS"),
                                 ("BROOD_PROXMOX_TOKENS", "BROOD_PROXMOX_POOLS")):
            for empty in (None, "", " \t\n"):
                with self.subTest(missing=missing, empty=empty):
                    settings = {present: "configuration-sentinel"}
                    if empty is not None:
                        settings[missing] = empty
                    deploy = self.deploy(settings)
                    deploy.cluster.shell.output.side_effect = AssertionError("unexpected native call")
                    with patch.object(go_server, "Infrastructure") as infrastructure:
                        with self.assertRaises(RuntimeError) as raised:
                            deploy.run()
                    self.assertIn(missing, str(raised.exception))
                    self.assertNotIn("configuration-sentinel", str(raised.exception))
                    infrastructure.assert_not_called()
                    deploy.cluster.check.assert_not_called()
                    deploy.cluster.shell.output.assert_not_called()

    def test_malformed_ownership_response_refuses_before_build(self):
        for response in ("not json", "[]", '{"metadata":null}', '{"metadata":{"labels":[]}}'):
            with self.subTest(response=response):
                deploy = self.deploy()
                deploy.cluster.shell.output.return_value = (0, response, "")
                with patch.object(go_server, "Infrastructure"):
                    with self.assertRaisesRegex(RuntimeError, "cannot check ownership"):
                        deploy.run()
                deploy.cluster.shell.output.assert_called_once()

    # FEATURE: Runtime Proxmox получает отдельные server-only настройки и точный egress.
    # SPEC: Парные origin/relay, отказ без утечки, DNS seam, /32 и /128 на точный порт.
    # IMPLEMENTATION: Явные Secret refs сервера, инъекция resolver и отдельные egress-правила.
    # RESULT: IMPLEMENTED — старый config-only режим сохраняется без DNS и API egress.
    # STATUS: DONE
    def runtime_settings(self, endpoint="https://pve.invalid:8006"):
        return {
            "BROOD_PROXMOX_POOLS": json.dumps({"lab": {
                "endpoint": endpoint, "template": "local:vztmpl/test.tar.zst",
                "templateDigest": "sha256:" + "d" * 64,
                "capacity": {"actors": 1, "cpuMillicores": "1000", "memoryBytes": "1073741824", "diskGiB": 8},
                "caPEM": "private-ca-sentinel"}}),
            "BROOD_PROXMOX_TOKENS": '{"lab":"private-token-sentinel"}',
            "BROOD_PUBLIC_ORIGIN": "https://brood.invalid",
            "BROOD_PROXMOX_RELAY_SECRET": "relay-secret-" * 4,
        }

    def test_runtime_secret_refs_checksum_and_exact_server_egress(self):
        settings = self.runtime_settings()
        answers = [(socket.AF_INET6, socket.SOCK_STREAM, 6, "", ("fd00::12", 8006, 0, 0)),
                   (socket.AF_INET, socket.SOCK_STREAM, 6, "", ("10.23.4.5", 8006)),
                   (socket.AF_INET, socket.SOCK_STREAM, 6, "", ("10.23.4.5", 8006))]
        with patch.object(go_server.socket, "getaddrinfo", return_value=answers) as resolver:
            first = self.capture_release(settings)
            second = self.capture_release({**settings, "BROOD_PROXMOX_RELAY_SECRET": "rotated-relay-" * 4})
        self.assertEqual(resolver.call_count, 2)
        resolver.assert_called_with("pve.invalid", 8006, type=socket.SOCK_STREAM)
        baseline = next(o for o in render("localhost:5002/brood-go@sha256:" + "a" * 64)
                        if o["kind"] == "NetworkPolicy")
        for objects in (first, second):
            policy = next(o for o in objects if o["kind"] == "NetworkPolicy")
            self.assertEqual(policy["spec"]["podSelector"], {"matchLabels": {"app": "brood-go"}})
            self.assertEqual(policy["spec"]["egress"], baseline["spec"]["egress"] + [
                {"to": [{"ipBlock": {"cidr": ip}}], "ports": [{"port": 8006, "protocol": "TCP"}]}
                for ip in ("10.23.4.5/32", "fd00::12/128")])
            deployment = next(o for o in objects if o["kind"] == "Deployment")
            pod = deployment["spec"]["template"]["spec"]
            self.assertNotIn("hostNetwork", pod)
            env = {e["name"]: e for e in pod["containers"][0]["env"]}
            secret = next(o for o in objects if o["metadata"]["name"] == "brood-go-env")
            self.assertNotIn("stringData", secret)
            for key in settings:
                self.assertIn(key, secret["data"])
                self.assertEqual(env[key], {"name": key, "valueFrom": {
                    "secretKeyRef": {"name": "brood-go-env", "key": key}}})
            for obj in objects:
                if obj["kind"] not in ("Deployment", "Secret"):
                    self.assertNotIn("BROOD_PROXMOX_", json.dumps(obj))
            self.assertFalse(any(o["kind"] in ("Actor", "WorkerPool", "StatefulSet") for o in objects))
        templates = [next(o for o in objects if o["kind"] == "Deployment")["spec"]["template"]
                     for objects in (first, second)]
        self.assertNotEqual(templates[0]["metadata"]["annotations"], templates[1]["metadata"]["annotations"])

    def test_runtime_invalid_settings_fail_before_native_calls_without_values(self):
        settings = self.runtime_settings()
        variants = [{**settings, key: value} for key, value in (
            ("BROOD_PUBLIC_ORIGIN", ""), ("BROOD_PROXMOX_RELAY_SECRET", ""),
            ("BROOD_PROXMOX_RELAY_SECRET", "short"), ("BROOD_PROXMOX_RELAY_SECRET", "x" * 32),
            *(("BROOD_PUBLIC_ORIGIN", origin) for origin in (
                "http://brood.invalid", "https://user:password@brood.invalid", "https://brood.invalid/path",
                "https://brood.invalid?", "https://brood.invalid#", "https://brood.invalid:0",
                "https://brood.invalid:", "https://brood.invalid:65536", "https://brood.invalid/",
                "https://[2001:db8::1]junk", "https://brood.invalid\u007f")))]
        for values in variants:
            with self.subTest(keys=list(values)):
                deploy = self.deploy(values)
                with patch.object(go_server, "Infrastructure") as infrastructure:
                    with self.assertRaises(RuntimeError) as raised:
                        deploy.run()
                self.assertIn("BROOD_", str(raised.exception))
                for value in values.values():
                    if value:
                        self.assertNotIn(value, str(raised.exception))
                infrastructure.assert_not_called()
                deploy.cluster.shell.output.assert_not_called()

    def test_runtime_resolver_seam_defaults_deduplicates_and_sanitizes(self):
        resolver = Mock(return_value=[(socket.AF_INET, socket.SOCK_STREAM, 6, "", ("10.23.4.5", 443))])
        deploy = self.deploy(self.runtime_settings("https://pve.invalid"))
        deploy.resolver = resolver
        with patch.object(go_server, "Infrastructure") as infrastructure:
            infrastructure.side_effect = RuntimeError("stop after validation")
            with self.assertRaisesRegex(RuntimeError, "stop after validation"):
                deploy.run()
        resolver.assert_called_once_with("pve.invalid", 443, type=socket.SOCK_STREAM)
        for address in ("127.0.0.1", "0.0.0.0", "224.0.0.1", "169.254.1.2", "::1", "::", "ff02::1", "fe80::1", "::ffff:127.0.0.1"):
            resolver.return_value = [(socket.AF_INET, socket.SOCK_STREAM, 6, "", (address, 443))]
            with self.subTest(address=address), self.assertRaisesRegex(RuntimeError, "BROOD_PROXMOX_POOLS"):
                deploy.run()
        resolver.return_value = []
        with self.assertRaisesRegex(RuntimeError, "BROOD_PROXMOX_POOLS"):
            deploy.run()
        resolver.side_effect = socket.gaierror("private-token-sentinel private-ca-sentinel")
        with self.assertRaises(RuntimeError) as raised:
            deploy.run()
        self.assertNotIn("sentinel", str(raised.exception))

    def test_optional_runtime_pair_uses_utf8_bytes_and_removal_clears_refs(self):
        settings = {"BROOD_PUBLIC_ORIGIN": "https://brood.invalid:8443", "BROOD_PROXMOX_RELAY_SECRET": "я" * 16}
        with patch.object(go_server.socket, "getaddrinfo", side_effect=AssertionError("unexpected DNS")):
            configured = self.capture_release(settings)
            removed = self.capture_release({})
        for objects, expected in ((configured, settings), (removed, {})):
            secret = next(o for o in objects if o["metadata"]["name"] == "brood-go-env")
            decoded = {key: base64.b64decode(value).decode("utf-8") for key, value in secret["data"].items()}
            self.assertEqual(decoded, {"BROOD_SESSION_SECRET": "x" * 32, **expected})
            deployment = next(o for o in objects if o["kind"] == "Deployment")
            env = {e["name"] for e in deployment["spec"]["template"]["spec"]["containers"][0]["env"]}
            self.assertEqual(set(settings) & env, set(expected))

    def test_config_only_does_not_resolve_or_open_api_egress(self):
        with patch.object(go_server.socket, "getaddrinfo", side_effect=AssertionError("unexpected DNS")):
            objects = self.capture_release({"BROOD_PROXMOX_POOLS": '{"lab":{"endpoint":"https://pve.invalid:8006"}}',
                                            "BROOD_PROXMOX_TOKENS": '{"lab":"token"}'})
        self.assertEqual(next(o for o in objects if o["kind"] == "NetworkPolicy"),
                         next(o for o in render("localhost:5002/brood-go@sha256:" + "a" * 64)
                              if o["kind"] == "NetworkPolicy"))
