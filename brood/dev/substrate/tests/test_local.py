"""Жизненный цикл локального Substrate без Docker: порт hack/substrate_test.sh.

Docker, kind и kubectl -- понарошку (World): контейнеры по имени, команды
пишутся в журнал. Главное, что здесь проверяется, -- отказ до первой правки:
имена контейнеров постоянные, и ошибка в проверке владения остановила бы
или удалила чужой кластер.
"""
import copy
import hashlib
import json
import os
import tempfile
import unittest

from brood.dev.substrate import local
from brood.dev.substrate.local import NODE, REGISTRY

NODE_ID = "06c7cd231d68"
REGISTRY_ID = "0d1cb4c3d1f2"
KUBECTL = b"kubectl binary"


def node(labels=None):
    return {"id": NODE_ID, "labels": {"io.x-k8s.kind.cluster": "substrate", **(labels or {})}}


def registry(labels=None, network=True):
    return {"id": REGISTRY_ID, "labels": {"created-by": "substrate-local", **(labels or {})},
            "network": network}


def atelet():
    return {"metadata": {"uid": "atelet-uid", "resourceVersion": "17"},
            "spec": {"template": {"spec": {"containers": [
                {"name": "sidecar", "args": ["--unrelated"]},
                {"name": "atelet", "args": ["--log-level=debug",
                    "--localhost-registry-replacement=kind-registry:5000", "--tls=on"]}]}}}}


# BUG: containerd знает substrate-registry, но atelet обращается к kind-registry.
# HYPOTHESIS: kind overlay Substrate зашивает своё имя вместо реестра установки.
# SOLUTION: после upstream deploy заменить только флаг atelet с uid/version и args test.
# RESULT: FIXED — containerd и atelet используют один реестр; 185 тестов проходят.
# STATUS: FIXED
class RegistryPatchTests(unittest.TestCase):
    def test_patch_changes_only_the_atelet_registry_flag_with_preconditions(self):
        original = atelet()
        before = copy.deepcopy(original)
        patch = local.atelet_registry_patch(original)
        self.assertEqual(original, before)
        self.assertEqual(patch, [
            {"op": "test", "path": "/metadata/uid", "value": "atelet-uid"},
            {"op": "test", "path": "/metadata/resourceVersion", "value": "17"},
            {"op": "test", "path": "/spec/template/spec/containers/1/args", "value": [
                "--log-level=debug", "--localhost-registry-replacement=kind-registry:5000",
                "--tls=on"]},
            {"op": "replace", "path": "/spec/template/spec/containers/1/args/1",
             "value": "--localhost-registry-replacement=substrate-registry:5000"}])

    def test_already_correct_endpoint_needs_no_rollout(self):
        value = atelet()
        value["spec"]["template"]["spec"]["containers"][1]["args"][1] = (
            "--localhost-registry-replacement=substrate-registry:5000")
        self.assertEqual(local.atelet_registry_patch(value), [])

    def test_upstream_shape_drift_is_refused(self):
        for value in ({}, {"spec": {"template": {"spec": {"containers": []}}}},
                      {"metadata": {"uid": "u", "resourceVersion": "v"}, "spec": {
                          "template": {"spec": {"containers": [{"name": "atelet", "args": []}]}}}}):
            with self.subTest(value=value), self.assertRaisesRegex(RuntimeError, "atelet"):
                local.atelet_registry_patch(value)


class World:
    """Docker, kind, kubectl и go понарошку. calls -- журнал команд строками,
    программа -- по имени файла: `kind.sh create cluster ...`."""

    def __init__(self, containers=None, ipv6=True, api_failures=0, docker=True,
                 tools=("go",)):
        self.containers = dict(containers or {})
        self.ipv6, self.api_failures, self.docker, self.tools = ipv6, api_failures, docker, tools
        self.calls, self.stdin = [], {}

    def _log(self, argv):
        words = (os.path.basename(argv[0]), *argv[1:])
        self.calls.append(" ".join(words))
        return words

    def which(self, tool):
        return tool in self.tools

    def output(self, *argv):
        words = self._log(argv)
        match words:
            case ("docker", "info"):
                return (0, "", "") if self.docker else (1, "", "Cannot connect to the Docker daemon\n")
            case ("docker", "container", "inspect", name):
                return (0 if name in self.containers else 1), "", ""
            case ("docker", "inspect", "-f", fmt, name):
                c = self.containers[name]
                if fmt == "{{.Id}}":
                    return 0, c["id"] + "\n", ""
                if "Labels" in fmt:
                    return 0, c["labels"].get(fmt.split('"')[1], "<no value>") + "\n", ""
                return 0, ('{"NetworkID":"x"}' if c.get("network") else "null") + "\n", ""
            case ("docker", "exec", _, "test", "-d", "/proc/sys/net/ipv6"):
                return (0 if self.ipv6 else 1), "", ""
            case ("kubectl", _, "get", "--raw=/readyz", _):
                if self.api_failures:
                    self.api_failures -= 1
                    return 1, "", "connection refused\n"
                return 0, "ok", ""
            case ("kubectl", _, "get", "daemonset/atelet", "-n", "ate-system", "-o", "json", _):
                return 0, json.dumps(atelet()), ""
            case ("go", "env", name):
                return 0, {"GOOS": "linux", "GOARCH": "amd64"}[name] + "\n", ""
        raise AssertionError(f"unexpected output() call: {' '.join(words)}")

    def run(self, *argv, stdin=None):
        words = self._log(argv)
        if stdin is not None:
            self.stdin[words[-1]] = stdin
        match words:
            case ("docker", "run", *_):
                self.containers[REGISTRY] = registry(network=False)
            case ("kind.sh", "create", "cluster", *_):
                self.containers[NODE] = node()
            case ("kind.sh", "delete", "cluster", *_):
                self.containers.pop(NODE, None)
            case ("docker", "rm", "-f", name):
                del self.containers[name]
            case ("docker", "network", "connect", "kind", name):
                self.containers[name]["network"] = True


class LifecycleTests(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.source = tmp.name
        self.state = local.state_dir(self.source)
        self.sleeps = []
        self.now = [0.0]

    def cluster(self, world, fetch=None):
        return local.LocalCluster(self.source, world, fetch=fetch or self.fetch,
                                  sleep=self.sleep, clock=lambda: self.now[0])

    def sleep(self, seconds):
        self.sleeps.append(seconds)
        self.now[0] += seconds

    @staticmethod
    def fetch(url, path):
        with open(path, "wb") as f:
            f.write(hashlib.sha256(KUBECTL).hexdigest().encode() + b"\n"
                    if url.endswith(".sha256") else KUBECTL)

    def installed(self, node_id=NODE_ID, registry_id=REGISTRY_ID):
        os.makedirs(self.state)
        for name, text in (("owned", ""), ("node-id", node_id + "\n"),
                           ("registry-id", registry_id + "\n"), ("kubeconfig", "config")):
            with open(os.path.join(self.state, name), "w") as f:
                f.write(text)
        kubectl = os.path.join(self.state, "kubectl")
        with open(kubectl, "w") as f:
            f.write("#!/bin/sh\n")
        os.chmod(kubectl, 0o755)

    def assertUntouched(self, world):
        """Ни одной правки: команда остановилась на проверках."""
        changes = [c for c in world.calls
                   if c.split()[:2] in (["docker", "stop"], ["docker", "start"], ["docker", "rm"],
                                        ["docker", "run"], ["kind.sh", "delete"],
                                        ["kind.sh", "create"])]
        self.assertEqual(changes, [])

    # ─── start, stop ─────────────────────────────────────────────────────
    def test_start_without_ownership_refuses(self):
        world = World({NODE: node(), REGISTRY: registry()})
        with self.assertRaisesRegex(RuntimeError, "not installed: run brood substrate install"):
            self.cluster(world).start()
        self.assertUntouched(world)

    def test_stop(self):
        self.installed()
        world = World({NODE: node(), REGISTRY: registry()})
        self.cluster(world).stop()
        self.assertEqual([c for c in world.calls if c.startswith("docker stop")],
                         [f"docker stop {NODE}", f"docker stop {REGISTRY}"])

    def test_start_waits_for_api(self):
        self.installed()
        world = World({NODE: node(), REGISTRY: registry()}, api_failures=1)
        self.cluster(world).start()
        self.assertEqual(self.sleeps, [1])
        changes = [c for c in world.calls if not c.startswith(("docker container inspect",
                                                                "docker inspect", "docker info"))]
        self.assertEqual(changes, [
            f"docker start {REGISTRY}",
            f"docker start {NODE}",
            f"docker exec {NODE} test -d /proc/sys/net/ipv6",
            f"docker exec {NODE} sysctl net.ipv4.conf.all.proxy_arp=1",
            "kubectl --context=kind-substrate get --raw=/readyz --request-timeout=5s",
            "kubectl --context=kind-substrate get --raw=/readyz --request-timeout=5s",
            "kubectl --context=kind-substrate wait --for=condition=Ready node --all --timeout=300s",
            "kubectl --context=kind-substrate wait --for=condition=Available deployment --all "
            "-n ate-system --timeout=600s",
            "kubectl --context=kind-substrate rollout status daemonset/atelet -n ate-system "
            "--timeout=600s",
        ])

    def test_api_never_ready_refuses(self):
        self.installed()
        world = World({NODE: node(), REGISTRY: registry()}, api_failures=10**6)
        with self.assertRaisesRegex(RuntimeError, "did not become ready within 300 seconds"):
            self.cluster(world).start()
        self.assertFalse(any("wait --for" in c for c in world.calls))

    def test_start_incomplete_refuses(self):
        self.installed()
        world = World({NODE: node()})
        with self.assertRaisesRegex(RuntimeError, "incomplete: run brood substrate install"):
            self.cluster(world).start()
        self.assertUntouched(world)

    def test_docker_down_refuses_first(self):
        self.installed()
        world = World({NODE: node(), REGISTRY: registry()}, docker=False)
        for command in ("install", "start", "stop", "uninstall"):
            with self.subTest(command=command):
                with self.assertRaisesRegex(RuntimeError, "docker is not available: Cannot "
                                                          "connect to the Docker daemon"):
                    getattr(self.cluster(world), command)()
        self.assertEqual(set(world.calls), {"docker info"})

    # ─── install ─────────────────────────────────────────────────────────
    def test_install_from_scratch(self):
        world = World()
        c = self.cluster(world)
        c.install()
        calls = world.calls
        self.assertIn(f"docker run -d --restart=no --label created-by=substrate-local "
                      f"-p 127.0.0.1:5002:5000 --name {REGISTRY} {local.REGISTRY_IMAGE}", calls)
        self.assertIn(f"kind.sh create cluster --name substrate --config {local.KIND_CONFIG} "
                      f"--wait 180s", calls)
        self.assertIn(f"docker network connect kind {REGISTRY}", calls)
        self.assertEqual(world.stdin["/etc/containerd/certs.d/localhost:5002/hosts.toml"],
                         '[host."http://substrate-registry:5000"]\n')
        self.assertIn("kind.sh export kubeconfig --name substrate", calls)
        installer = next(c for c in calls if c.startswith("install-ate-kind.sh"))
        for flag in ("deploy ate-system", '--credential-provider={"name":"k8s.io"}',
                     "--ingress-allowed-spiffe-ids=spiffe://cluster.local/ns/ate-system/sa/ate-client",
                     "--rollout-timeout=10m", "--namespace=ate-system", "--atenet-dataplane=envoy"):
            self.assertIn(flag, installer)
        self.assertIn(f"go build -o {c.kubectl_ate} ./cmd/kubectl-ate", calls)
        # Раскатка -- после проверки ядра, готовность -- после раскатки.
        self.assertLess(calls.index(f"docker exec {NODE} test -d /proc/sys/net/ipv6"),
                        calls.index(installer))
        self.assertEqual(calls[-1], "kubectl --context=kind-substrate rollout status "
                                    "daemonset/atelet -n ate-system --timeout=600s")
        with open(c.ids[NODE]) as f:
            self.assertEqual(f.read(), NODE_ID + "\n")
        with open(c.ids[REGISTRY]) as f:
            self.assertEqual(f.read(), REGISTRY_ID + "\n")
        with open(c.kubectl, "rb") as f:
            self.assertEqual(f.read(), KUBECTL)
        self.assertTrue(os.access(c.kubectl, os.X_OK))
        self.assertTrue(os.path.isfile(c.owned))

    def test_install_reconciles_actor_registry_after_upstream_and_before_readiness(self):
        world = World()
        self.cluster(world).install()
        patches = [call for call in world.calls if "patch daemonset/atelet" in call]
        self.assertEqual(len(patches), 1)
        self.assertIn("--type=json", patches[0])
        self.assertIn("substrate-registry:5000", patches[0])
        installer = next(call for call in world.calls if call.startswith("install-ate-kind.sh"))
        readiness = next(call for call in world.calls if "rollout status daemonset/atelet" in call)
        self.assertLess(world.calls.index(installer), world.calls.index(patches[0]))
        self.assertLess(world.calls.index(patches[0]), world.calls.index(readiness))

    def test_install_again_redeploys_into_same_cluster(self):
        self.installed()
        world = World({NODE: node(), REGISTRY: registry()})
        self.cluster(world, fetch=self.no_fetch).install()
        self.assertIn(f"docker start {REGISTRY}", world.calls)
        self.assertNotIn(f"docker network connect kind {REGISTRY}", world.calls)
        self.assertFalse(any(c.startswith(("docker run", "kind.sh create")) for c in world.calls))
        self.assertTrue(any(c.startswith("install-ate-kind.sh deploy") for c in world.calls))

    @staticmethod
    def no_fetch(url, path):
        raise AssertionError(f"kubectl is already there, fetched {url}")

    def test_install_without_ipv6_refuses_before_deploy(self):
        world = World(ipv6=False)
        with self.assertRaisesRegex(RuntimeError, "IPv6 in the Docker kernel"):
            self.cluster(world).install()
        self.assertFalse(any(c.startswith("install-ate-kind.sh") for c in world.calls))

    def test_install_over_foreign_containers_refuses(self):
        world = World({NODE: node()})
        with self.assertRaisesRegex(RuntimeError, f"container {NODE} already exists"):
            self.cluster(world).install()
        self.assertUntouched(world)
        self.assertFalse(os.path.exists(self.state))

    def test_install_needs_go(self):
        world = World(tools=())
        with self.assertRaisesRegex(RuntimeError, "install needs go on PATH"):
            self.cluster(world).install()
        self.assertFalse(os.path.exists(self.state))

    def test_kubectl_checksum_mismatch_refuses(self):
        def corrupt(url, path):
            with open(path, "wb") as f:
                f.write(b"0" * 64 + b"\n" if url.endswith(".sha256") else KUBECTL)
        world = World()
        c = self.cluster(world, fetch=corrupt)
        with self.assertRaisesRegex(RuntimeError, "kubectl v1.37.1 checksum mismatch"):
            c.install()
        self.assertFalse(os.path.exists(c.kubectl))
        self.assertEqual(os.listdir(self.state), ["owned"])
        self.assertUntouched(world)

    # ─── uninstall и владение ────────────────────────────────────────────
    def test_uninstall_foreign_registry_refuses(self):
        self.installed()
        world = World({NODE: node(), REGISTRY: registry({"created-by": "someone-else"})})
        with self.assertRaisesRegex(RuntimeError, f"refusing to modify {REGISTRY}"):
            self.cluster(world).uninstall()
        self.assertUntouched(world)

    def test_uninstall_foreign_node_refuses(self):
        self.installed()
        world = World({NODE: node({"io.x-k8s.kind.cluster": "other"}), REGISTRY: registry()})
        with self.assertRaisesRegex(RuntimeError, f"refusing to modify {NODE}"):
            self.cluster(world).uninstall()
        self.assertUntouched(world)

    def test_replaced_container_refuses(self):
        """Тот же ярлык, другой контейнер: запись владения не сходится."""
        for node_id, registry_id in (("", REGISTRY_ID), ("ffff", REGISTRY_ID),
                                     (NODE_ID, "")):
            with self.subTest(node_id=node_id, registry_id=registry_id):
                self.setUp()
                self.installed(node_id, registry_id)
                world = World({NODE: node(), REGISTRY: registry()})
                with self.assertRaisesRegex(RuntimeError, "no matching ownership record"):
                    self.cluster(world).uninstall()
                self.assertUntouched(world)

    def test_uninstall(self):
        self.installed()
        world = World({NODE: node(), REGISTRY: registry()})
        self.cluster(world).uninstall()
        self.assertIn("kind.sh delete cluster --name substrate", world.calls)
        self.assertIn(f"docker rm -f {REGISTRY}", world.calls)
        self.assertEqual(world.containers, {})
        # kubectl остаётся: uninstall удаляет данные кластера, не инструменты.
        self.assertEqual(os.listdir(self.state), ["kubectl"])


class EnvironmentTests(unittest.TestCase):
    def test_environment(self):
        env = local.environment({"PATH": "/usr/bin", "ATE_CONFIG": "/unrelated.yaml",
                                 "HOME": "/home/u"}, "/src")
        self.assertEqual(env, {
            "PATH": "/src/bin/substrate:/src/bin:/usr/bin",
            "HOME": "/home/u",
            "KUBECONFIG": "/src/bin/substrate/kubeconfig",
            "KIND_CLUSTER_NAME": "substrate",
            "KUBECTL_CONTEXT": "kind-substrate",
            "KO_DOCKER_REPO": "localhost:5002",
        })

    def test_empty_path_adds_no_current_directory(self):
        self.assertEqual(local.environment({}, "/src")["PATH"], "/src/bin/substrate:/src/bin")

    def test_checkout(self):
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaisesRegex(RuntimeError, "is not a Substrate checkout: no hack/kind.sh"):
                local.checkout(tmp)
            for marker in local.MARKERS:
                os.makedirs(os.path.dirname(os.path.join(tmp, marker)), exist_ok=True)
                open(os.path.join(tmp, marker), "w").close()
            self.assertEqual(local.checkout(os.path.join(tmp, ".")), tmp)

    def test_kind_config_is_packaged(self):
        with open(local.KIND_CONFIG) as f:
            self.assertIn("kind: Cluster", f.read())

    def test_reason(self):
        self.assertEqual(local.reason("\n  boom\nmore\n", 3), "boom")
        self.assertEqual(local.reason("", 3), "exit 3")
