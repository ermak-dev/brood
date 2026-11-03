"""Go-выкладка хранит SQLite на PVC и не меняет другие runtime и данные."""
import base64
import hashlib
import json
import socket
from pathlib import Path
import unittest
from unittest.mock import Mock, call, patch

from brood.dev.deploy import go_server


GIT_USER = "synthetic-git-user"
GIT_TOKEN = "synthetic-git-token-sentinel"
REMOTES = {"https://git.example.test/group/repo.git": {"username": GIT_USER, "token": GIT_TOKEN},
           "https://git.example.test/group/other": {},
           "https://lan.example.test/team/tools": {}}


def render(image, **options):
    return go_server.manifests(image, {"gvisor": "localhost:5002/ateworker-gvisor@sha256:" + "b" * 64}, **options)


def answer(address):
    family = socket.AF_INET6 if ":" in address else socket.AF_INET
    return (family, socket.SOCK_STREAM, 6, "", (address, 443))


# FEATURE: BROOD_GIT_REMOTES — remotes репозиториев проекта, секрет установки.
# SPEC: ключ — канонический HTTPS URL, значение — {} или username/token видимым ASCII;
# отказ называет настройку, но не значения: в них токены и адреса.
class GitRemotesSettingTests(unittest.TestCase):
    def refused(self, raw):
        with self.assertRaises(RuntimeError) as raised:
            go_server.git_remote_hosts(raw)
        message = str(raised.exception)
        self.assertIn("BROOD_GIT_REMOTES", message)
        for value in (GIT_TOKEN, GIT_USER, "example.test", "sentinel"):
            self.assertNotIn(value, message)
        return message

    def test_valid_remotes_yield_distinct_sorted_hosts(self):
        self.assertEqual(go_server.git_remote_hosts(json.dumps(REMOTES)), ["git.example.test", "lan.example.test"])
        self.assertEqual(go_server.git_remote_hosts(json.dumps({
            "https://git-01.lan/~owner/Group/Repo_2.git": {"username": "oauth2", "token": "glpat-A_b.c~!$"},
            "https://gitlab/team/repo": {}})), ["git-01.lan", "gitlab"])

    def test_absent_or_empty_setting_has_no_remotes(self):
        for raw in (None, "", "{}"):
            with self.subTest(raw=raw):
                self.assertEqual(go_server.git_remote_hosts(raw), [])

    def test_non_canonical_origins_are_refused_without_values(self):
        for origin in ("http://git.example.test/repo", "ssh://git.example.test/repo",
                       "git@git.example.test:repo.git", "HTTPS://git.example.test/repo",
                       "https://Git.example.test/repo", "https://git.example.test./repo",
                       "https://git.example.test:443/repo", "https://git.example.test:8443/repo",
                       "https://git.example.test:/repo", "https://user@git.example.test/repo",
                       "https://user:pw@git.example.test/repo", "https://git.example.test/repo?ref=main",
                       "https://git.example.test/repo#main", "https://git.example.test",
                       "https://git.example.test/", "https://git.example.test/repo/",
                       "https://git.example.test//repo", "https://git.example.test/a/../repo",
                       "https://git.example.test/./repo", "https://git.example.test/re%2Fpo",
                       "https://git.example.test/re po", "https://git.example.test/re\tpo",
                       "https://git.example.test/re\\po", "https://git.example.test/репо",
                       "https://гит.example.test/repo", "https://10.0.0.5/repo", "https://127.1/repo",
                       "https://[fd00::5]/repo", "https://-git.example.test/repo",
                       "https://git_lab.example.test/repo", "https://" + "a" * 64 + ".example.test/repo",
                       "https://git.example.test/" + "r" * 1024, "", " https://git.example.test/repo"):
            with self.subTest(origin=origin):
                message = self.refused(json.dumps({origin: {}}))
                if origin:
                    self.assertNotIn(origin, message)

    def test_credentials_are_empty_or_visible_username_and_token(self):
        for credentials in ({"username": GIT_USER}, {"token": GIT_TOKEN},
                            {"username": GIT_USER, "token": GIT_TOKEN, "extra": "sentinel"},
                            {"username": "", "token": GIT_TOKEN}, {"username": GIT_USER, "token": ""},
                            {"username": GIT_USER, "token": GIT_TOKEN + " x"},
                            {"username": GIT_USER, "token": GIT_TOKEN + "\t"},
                            {"username": GIT_USER, "token": GIT_TOKEN + "\n"},
                            {"username": GIT_USER, "token": GIT_TOKEN + "ж"},
                            {"username": 7, "token": GIT_TOKEN}, {"username": GIT_USER, "token": None},
                            {"password": GIT_TOKEN}, None, [], GIT_TOKEN, True):
            with self.subTest(credentials=credentials):
                self.refused(json.dumps({"https://git.example.test/repo": credentials}))

    def test_malformed_or_duplicated_json_is_refused(self):
        for raw in ("sentinel", " ", "[]", "null", "1", '"https://git.example.test/repo"',
                    '{"https://git.example.test/repo": {}, "https://git.example.test/repo": {}}',
                    '{"https://git.example.test/repo": {"username": "u", "token": "a", "token": "'
                    + GIT_TOKEN + '"}}', "[" * 100000):
            with self.subTest(raw=raw[:80]):
                self.refused(raw)

    def test_egress_allows_operator_lan_hosts_on_https_only(self):
        answers = {"git.example.test": [answer("203.0.113.7")],
                   "lan.example.test": [answer("192.168.10.20"), answer("10.1.2.3"), answer("fd00::20"),
                                        answer("::ffff:192.168.10.20")]}
        resolver = Mock(side_effect=lambda host, port, **kwargs: answers[host])
        self.assertEqual(go_server.git_remote_egress(["git.example.test", "lan.example.test"], resolver), [
            {"to": [{"ipBlock": {"cidr": cidr}}], "ports": [{"port": 443, "protocol": "TCP"}]}
            for cidr in ("10.1.2.3/32", "192.168.10.20/32", "203.0.113.7/32", "fd00::20/128")])
        self.assertCountEqual(resolver.call_args_list, [
            call(host, 443, type=socket.SOCK_STREAM) for host in ("git.example.test", "lan.example.test")])
        self.assertEqual(go_server.git_remote_egress([], Mock(side_effect=AssertionError("unexpected DNS"))), [])

    def test_egress_refuses_unsafe_or_failed_resolution_without_values(self):
        for address in ("127.0.0.1", "::1", "0.0.0.0", "::", "169.254.169.254", "fe80::1", "224.0.0.1",
                        "ff02::1", "240.0.0.1", "::ffff:127.0.0.1", "fe80::1%eth0"):
            with self.subTest(address=address):
                with self.assertRaises(RuntimeError) as raised:
                    go_server.git_remote_egress(["git.example.test"], Mock(return_value=[
                        answer("192.168.10.20"), answer(address)]))
                self.assertIn("BROOD_GIT_REMOTES", str(raised.exception))
                self.assertNotIn("example.test", str(raised.exception))
        for resolver in (Mock(return_value=[]), Mock(side_effect=socket.gaierror(GIT_TOKEN + " git.example.test"))):
            with self.assertRaises(RuntimeError) as raised:
                go_server.git_remote_egress(["git.example.test"], resolver)
            self.assertIn("BROOD_GIT_REMOTES", str(raised.exception))
            self.assertNotIn("sentinel", str(raised.exception))
            self.assertNotIn("example.test", str(raised.exception))


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

    def test_server_receives_artifacts_directory_setting(self):
        # #156: выкладка публикует артефакты бутстрапа в PVC сервера; путь —
        # свойство машины деплоя, не секрет установки.
        deployment = next(item for item in render("localhost:5002/brood-go@sha256:" + "a" * 64)
                          if item["kind"] == "Deployment")
        env = {item["name"]: item.get("value")
               for item in deployment["spec"]["template"]["spec"]["containers"][0]["env"]}
        self.assertEqual(env.get("BROOD_ARTIFACTS_DIR"), "/data/artifacts")

    def test_bootstrap_artifact_publish_streams_all_three_binaries(self):
        """#156: после rollout выкладка отдаёт серверу bro/brood-guest/claude
        одним бинарным потоком brood artifacts; отказ любого шага — отказ
        выкладки, без частичной публикации."""
        import tempfile
        from pathlib import Path
        from unittest.mock import Mock
        project = Path(tempfile.mkdtemp()) / "project"
        (project / "brood/dev").mkdir(parents=True)
        import hashlib
        (project / "brood/dev/pins.json").write_text(json.dumps(
            {"CLAUDE_VERSION": "2.1.310",
             "CLAUDE_SHA256": hashlib.sha256(b"\x7fELFclaude-pinned").hexdigest()}))
        deploy = go_server.Deploy(Mock(), get={"BROOD_SESSION_SECRET": "x" * 32}.get, project=str(project))
        calls = []
        root = Path(tempfile.mkdtemp())
        (root).mkdir(exist_ok=True)
        bro = deploy.project / "go/bin/linux_amd64/bro"
        bro.parent.mkdir(parents=True, exist_ok=True)
        bro.write_bytes(b"\x7fELFbro-release")

        def output(*argv):
            calls.append(argv)
            if argv[0] == "curl":
                Path(argv[argv.index("-o") + 1]).write_bytes(b"\x7fELFclaude-pinned")
            if argv[0] == "go":
                Path(argv[argv.index("-o") + 1]).write_bytes(b"\x7fELFguest-built")
            return 0, "", ""

        streams = []
        deploy.cluster.shell.output.side_effect = output
        deploy.cluster.shell.output_bytes.side_effect = lambda argv, *, input: (streams.append((argv, input)), (0, b"", b""))[1]
        deploy.publish_bootstrap_artifacts(root)
        argv, stream = streams[0]
        self.assertEqual(argv[-4:], ("/brood", "artifacts", "--dir", "/data/artifacts"))
        self.assertIn(b"bro\0\x7fELFbro-release\0", stream)
        self.assertIn(b"brood-guest\0\x7fELFguest-built\0", stream)
        self.assertIn(b"claude\0\x7fELFclaude-pinned\0", stream)
        go_call = next(a for a in calls if a[0] == "go")
        self.assertIn("./go/cmd/brood-guest", go_call)

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

    def test_git_remotes_reach_server_only_through_secret_reference(self):
        def env(objects):
            deployment = next(item for item in objects if item["kind"] == "Deployment")
            return {e["name"]: e for e in deployment["spec"]["template"]["spec"]["containers"][0]["env"]}
        configured = env(render("localhost:5002/brood-go@sha256:" + "a" * 64, git_remotes=True))
        self.assertEqual(configured["BROOD_GIT_REMOTES"], {"name": "BROOD_GIT_REMOTES", "valueFrom": {
            "secretKeyRef": {"name": "brood-go-env", "key": "BROOD_GIT_REMOTES"}}})
        self.assertNotIn("BROOD_GIT_REMOTES", env(render("localhost:5002/brood-go@sha256:" + "a" * 64)))


class GoDeploymentTests(unittest.TestCase):
    def deploy(self, settings=None):
        cluster = Mock()
        workers = {"gvisor": "localhost:5002/ateworker-gvisor@sha256:" + "b" * 64}
        values = {"BROOD_SESSION_SECRET": "x" * 32, "WORKER_POOL_IMAGES": json.dumps(workers),
                  "BROOD_RELEASE_SHA": "c" * 40}
        values.update(settings or {})
        deploy = go_server.Deploy(cluster, get=lambda key: values.get(key, ""), project="/checkout")
        # Шаг артефактов (#156) требует реальных бинарей и машины деплоя:
        # в этих тестах он подменён, собственный тест ниже.
        deploy.publish_bootstrap_artifacts = lambda root: None
        return deploy

    # FEATURE: Ключ модели CLIProxyAPI доставляется только серверу.
    # SPEC: Secret reference без значения в pod spec; ротация меняет checksum.
    def test_model_client_key_is_server_only_and_rotates_checksum(self):
        settings = {"CLIPROXY_URL": "http://cliproxy:8317",
                    "CLIPROXY_MANAGEMENT_KEY": "synthetic-management",
                    "CLIPROXY_API_KEY": "synthetic-model-key"}
        def deployed(values):
            objects = self.capture_release(values)
            secret = next(o for o in objects if o["metadata"]["name"] == "brood-go-env")
            deployment = next(o for o in objects if o["kind"] == "Deployment")
            env = {entry["name"]: entry for entry in deployment["spec"]["template"]["spec"]["containers"][0]["env"]}
            self.assertNotIn(values["CLIPROXY_API_KEY"], json.dumps(deployment))
            self.assertEqual(base64.b64decode(secret["data"]["CLIPROXY_API_KEY"]).decode(), values["CLIPROXY_API_KEY"])
            self.assertEqual(env["CLIPROXY_API_KEY"]["valueFrom"]["secretKeyRef"],
                             {"name": "brood-go-env", "key": "CLIPROXY_API_KEY"})
            return deployment["spec"]["template"]["metadata"]["annotations"]["brood.dev/server-env-sha256"]
        self.assertNotEqual(deployed(settings), deployed({**settings, "CLIPROXY_API_KEY": "rotated-model-key"}))
        without = self.capture_release({key: value for key, value in settings.items() if key != "CLIPROXY_API_KEY"})
        self.assertNotIn("CLIPROXY_API_KEY", next(o for o in without if o["metadata"]["name"] == "brood-go-env")["data"])

    # FEATURE: Отдельный actor backend не заменяет установленный proxy панели.
    def test_actor_model_source_keeps_panel_proxy_and_adds_scoped_egress(self):
        values = {"CLIPROXY_URL": "http://cliproxy:8317", "CLIPROXY_MANAGEMENT_KEY": "control-secret",
                  "BROOD_ACTOR_LLM_URL": "https://models.example.test:9443",
                  "BROOD_ACTOR_LLM_API_KEY": "actor-client-secret",
                  "BROOD_ACTOR_LLM_CA_PEM_B64": base64.b64encode(b"synthetic-trust").decode()}
        resolver = Mock(return_value=[(2, 1, 6, "", ("192.168.50.8", 9443))])
        with patch("ssl.SSLContext.load_verify_locations") as trust:
            objects = self.capture_release(values, resolver=resolver)
            trust.assert_called_once_with(cadata="synthetic-trust")
        secret = next(o for o in objects if o["metadata"]["name"] == "brood-go-env")
        deployment = next(o for o in objects if o["kind"] == "Deployment")
        env = {item["name"]: item for item in deployment["spec"]["template"]["spec"]["containers"][0]["env"]}
        for key, value in values.items():
            self.assertIn(key, secret["data"])
            self.assertEqual(base64.b64decode(secret["data"][key]).decode(), value)
            self.assertNotIn(value, json.dumps(deployment))
            self.assertEqual(env[key]["valueFrom"]["secretKeyRef"], {"name": "brood-go-env", "key": key})
        policy = next(o for o in objects if o["kind"] == "NetworkPolicy" and o["metadata"]["name"] == "brood-go")
        self.assertIn({"to": [{"ipBlock": {"cidr": "192.168.50.8/32"}}],
                       "ports": [{"port": 9443, "protocol": "TCP"}]}, policy["spec"]["egress"])
        self.assertTrue(any(o["metadata"]["name"] == "brood-go-cliproxy" for o in objects))
        with self.assertRaisesRegex(RuntimeError, "BROOD_ACTOR_LLM_CA_PEM_B64"):
            self.capture_release({**values, "BROOD_ACTOR_LLM_CA_PEM_B64": "%%%"}, resolver=resolver)
        resolver.return_value = [(2, 1, 6, "", ("127.0.0.1", 9443))]
        with patch("ssl.SSLContext.load_verify_locations"), self.assertRaisesRegex(RuntimeError, "BROOD_ACTOR_LLM_URL"):
            self.capture_release(values, resolver=resolver)

    def test_actor_source_rotation_optional_trust_and_disable(self):
        values = {"BROOD_ACTOR_LLM_URL": "https://models.example.test",
                  "BROOD_ACTOR_LLM_API_KEY": "first-key"}
        resolver = Mock(return_value=[(socket.AF_INET, socket.SOCK_STREAM, 6, "", ("192.168.50.8", 443))])
        before = self.capture_release(values, resolver=resolver)
        after = self.capture_release({**values, "BROOD_ACTOR_LLM_API_KEY": "second-key"}, resolver=resolver)
        checksums = []
        for objects in (before, after):
            deployment = next(o for o in objects if o["kind"] == "Deployment")
            checksums.append(deployment["spec"]["template"]["metadata"]["annotations"]["brood.dev/server-env-sha256"])
            env = deployment["spec"]["template"]["spec"]["containers"][0]["env"]
            self.assertNotIn("BROOD_ACTOR_LLM_CA_PEM_B64", {item["name"] for item in env})
        self.assertNotEqual(*checksums)
        disabled = self.capture_release({})
        secret = next(o for o in disabled if o["metadata"]["name"] == "brood-go-env")
        self.assertFalse(set(go_server.ACTOR_LLM_SETTINGS) & secret["data"].keys())
        for change in ({"BROOD_ACTOR_LLM_URL": "http://models.example.test"},
                       {"BROOD_ACTOR_LLM_API_KEY": ""}, {"BROOD_ACTOR_LLM_API_KEY": "invalid key"},
                       {"BROOD_ACTOR_LLM_CA_PEM_B64": base64.b64encode(b"not a certificate").decode()}):
            with self.subTest(change=change), self.assertRaisesRegex(RuntimeError, "BROOD_ACTOR_LLM_"):
                self.capture_release({**values, **change}, resolver=resolver)

    # FEATURE: BROOD_GIT_REMOTES доходит до сервера Secret'ом и открывает точный HTTPS egress.
    # SPEC: полный JSON — только в data Secret; env — secretKeyRef; /32 и /128 на 443
    # каждого host; ротация токена меняет checksum; токен не попадает в argv, вывод и manifests.
    def test_git_remotes_reach_server_secret_and_open_exact_https_egress(self):
        raw = json.dumps(REMOTES)
        answers = {"git.example.test": [answer("203.0.113.7")],
                   "lan.example.test": [answer("192.168.10.20"), answer("fd00::20")]}
        resolver = Mock(side_effect=lambda host, port, **kwargs: answers[host])
        objects = self.capture_release({"BROOD_GIT_REMOTES": raw}, resolver=resolver, forbidden=(GIT_TOKEN, GIT_USER))
        self.assertCountEqual(resolver.call_args_list, [
            call(host, 443, type=socket.SOCK_STREAM) for host in ("git.example.test", "lan.example.test")])
        secret = next(o for o in objects if o["metadata"]["name"] == "brood-go-env")
        self.assertEqual(base64.b64decode(secret["data"]["BROOD_GIT_REMOTES"]).decode("utf-8"), raw)
        deployment = next(o for o in objects if o["kind"] == "Deployment")
        env = {e["name"]: e for e in deployment["spec"]["template"]["spec"]["containers"][0]["env"]}
        self.assertEqual(env["BROOD_GIT_REMOTES"], {"name": "BROOD_GIT_REMOTES", "valueFrom": {
            "secretKeyRef": {"name": "brood-go-env", "key": "BROOD_GIT_REMOTES"}}})
        baseline = next(o for o in render("localhost:5002/brood-go@sha256:" + "a" * 64)
                        if o["kind"] == "NetworkPolicy")
        policy = next(o for o in objects if o["kind"] == "NetworkPolicy" and o["metadata"]["name"] == "brood-go")
        self.assertEqual(policy["spec"]["egress"], baseline["spec"]["egress"] + [
            {"to": [{"ipBlock": {"cidr": cidr}}], "ports": [{"port": 443, "protocol": "TCP"}]}
            for cidr in ("192.168.10.20/32", "203.0.113.7/32", "fd00::20/128")])
        released = json.dumps(objects)
        for value in (GIT_TOKEN, GIT_USER, raw):
            self.assertNotIn(value, released)
        rotated = self.capture_release({"BROOD_GIT_REMOTES": raw.replace(GIT_TOKEN, "rotated-git-token")},
                                       resolver=resolver)
        self.assertNotEqual(*(next(o for o in release if o["kind"] == "Deployment")["spec"]["template"]
                              ["metadata"]["annotations"]["brood.dev/server-env-sha256"]
                              for release in (objects, rotated)))

    def test_git_remotes_absent_or_empty_keep_previous_release(self):
        with patch.object(go_server.socket, "getaddrinfo", side_effect=AssertionError("unexpected DNS")):
            baseline = self.capture_release({})
            for raw in ("", "{}"):
                with self.subTest(raw=raw):
                    self.assertEqual(self.capture_release({"BROOD_GIT_REMOTES": raw}), baseline)
        secret = next(o for o in baseline if o["metadata"]["name"] == "brood-go-env")
        self.assertNotIn("BROOD_GIT_REMOTES", secret["data"])

    def test_git_remotes_invalid_or_unsafe_refuse_before_native_calls_without_values(self):
        valid = json.dumps(REMOTES)
        cases = [(json.dumps({"https://git.example.test/repo": {"token": GIT_TOKEN}}), [answer("192.168.10.20")]),
                 (json.dumps({"https://git.example.test:8443/repo": {}}), [answer("192.168.10.20")]),
                 ("not json " + GIT_TOKEN, [answer("192.168.10.20")]),
                 (valid, [answer("127.0.0.1")]), (valid, [answer("169.254.169.254")]), (valid, [])]
        for raw, answers in cases:
            with self.subTest(raw=raw[:60], answers=answers):
                deploy = self.deploy({"BROOD_GIT_REMOTES": raw})
                deploy.resolver = Mock(return_value=answers)
                with patch.object(go_server, "Infrastructure") as infrastructure, \
                        patch("builtins.print") as printed, self.assertRaises(RuntimeError) as raised:
                    deploy.run()
                message = str(raised.exception)
                self.assertIn("BROOD_GIT_REMOTES", message)
                for value in (GIT_TOKEN, GIT_USER, "example.test"):
                    self.assertNotIn(value, message)
                printed.assert_not_called()
                infrastructure.assert_not_called()
                deploy.cluster.check.assert_not_called()
                deploy.cluster.shell.output.assert_not_called()
                if raw != valid:
                    deploy.resolver.assert_not_called()

    def capture_release(self, settings, resolver=None, forbidden=()):
        deploy = self.deploy(settings)
        if resolver is not None:
            deploy.resolver = resolver
        applied = []

        def output(*args):
            for key in ("BROOD_SESSION_SECRET", *go_server.PROXMOX_SETTINGS,
                        "BROOD_PUBLIC_ORIGIN", "BROOD_PROXMOX_RELAY_SECRET",
                        "CLIPROXY_URL", "CLIPROXY_MANAGEMENT_KEY", "CLIPROXY_API_KEY", *go_server.ACTOR_LLM_SETTINGS,
                        "BROOD_GIT_REMOTES"):
                value = settings.get(key)
                if value:
                    self.assertNotIn(value, args)
            for value in forbidden:
                self.assertFalse(any(value in str(arg) for arg in args))
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
