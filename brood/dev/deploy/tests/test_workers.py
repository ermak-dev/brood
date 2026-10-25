"""Выкладка начального пула без перезаписи состояния панели."""
import json
import os
import re
import tempfile
import unittest
from unittest import mock

from brood.dev.deploy import server
from brood.dev.substrate import local

# FEATURE: Начальный WorkerPool и узкий доступ Django к Kubernetes.
# SPEC: create-only, gvisor по digest, 2Gi; RBAC только WorkerPool;
# discovery до записи, точные IP/порты до и после DNAT, готовность Deployment.
# IMPLEMENTATION: pinned ko с кэшами; отдельные create и egress; узкая Role.
# RESULT: IMPLEMENTED — create-only не меняет пулы, discovery закрывается при ошибке.
# STATUS: DONE
DIGEST = "sha256:" + "a" * 64
IMAGE = f"{server.REGISTRY}/ateworker-gvisor@{DIGEST}"
SERVICE = {"spec": {"clusterIPs": ["10.96.0.1", "fd00::1"],
                    "ports": [{"name": "https", "port": 443, "protocol": "TCP"}]}}
ENDPOINTS = {"subsets": [{"addresses": [{"ip": "172.18.0.2"}],
                          "ports": [{"name": "https", "port": 6443, "protocol": "TCP"}]}]}


class WorkerManifestTests(unittest.TestCase):
    def test_initial_pool_shape(self):
        pool = server.initial_pool(IMAGE)
        self.assertEqual(pool, {
            "apiVersion": "ate.dev/v1", "kind": "WorkerPool",
            "metadata": {"name": "brood", "namespace": "brood"},
            "spec": {"replicas": 1, "workerImage": IMAGE,
                     "sandboxClasses": [{"name": "gvisor"}],
                     "template": {"resources": {
                         "requests": {"cpu": "250m", "memory": "2Gi"},
                         "limits": {"cpu": "2", "memory": "2Gi"}}}}})

    def test_exact_api_egress_before_and_after_dnat(self):
        rules = server.api_egress(SERVICE, ENDPOINTS)
        self.assertEqual(rules, [
            {"to": [{"ipBlock": {"cidr": "10.96.0.1/32"}},
                    {"ipBlock": {"cidr": "fd00::1/128"}}],
             "ports": [{"protocol": "TCP", "port": 443}]},
            {"to": [{"ipBlock": {"cidr": "172.18.0.2/32"}}],
             "ports": [{"protocol": "TCP", "port": 6443}]}])

    def test_unusable_discovery_is_explanatory(self):
        for service, endpoints in [({}, ENDPOINTS), (SERVICE, {}),
                                   (SERVICE, {"subsets": []}),
                                   (SERVICE, {"subsets": [{"ports": [None],
                                                           "addresses": [{"ip": "172.18.0.2"}]}]}),
                                   (SERVICE, {"subsets": [{"addresses": [{"ip": "bad"}],
                                                           "ports": [{"port": 6443}]}]})]:
            with self.subTest(service=service, endpoints=endpoints):
                with self.assertRaisesRegex(RuntimeError, "Kubernetes API discovery"):
                    server.api_egress(service, endpoints)

    def test_django_env_and_namespace_only_rbac(self):
        with open(os.path.join(server.MANIFESTS, "django.yaml")) as f:
            django = f.read()
        for key, value in {
            "KUBE_API_URL": "https://kubernetes.default.svc",
            "KUBE_API_TOKEN": "/var/run/secrets/kubernetes.io/serviceaccount/token",
            "KUBE_API_CA": "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt",
        }.items():
            self.assertIn(f"- name: {key}\n              value: {value}", django)
        with open(os.path.join(server.MANIFESTS, "kustomization.yaml")) as f:
            self.assertIn("  - worker-access.json", f.read())
        with open(os.path.join(server.MANIFESTS, "worker-access.json")) as f:
            objects = json.load(f)["items"]
        roles = [d for d in objects if d["kind"] in ("Role", "ClusterRole")]
        self.assertEqual(len(roles), 1)
        self.assertEqual(roles[0]["kind"], "Role")
        self.assertEqual(roles[0]["metadata"]["namespace"], "brood")
        self.assertEqual(roles[0]["rules"], [{"apiGroups": ["ate.dev"],
                          "resources": ["workerpools"],
                          "verbs": ["get", "list", "create", "update", "delete"]}])
        binding = next(d for d in objects if d["kind"] == "RoleBinding")
        self.assertEqual(binding["subjects"], [{"kind": "ServiceAccount", "name": "django",
                                                "namespace": "brood"}])
        self.assertEqual(binding["roleRef"]["name"], roles[0]["metadata"]["name"])
        self.assertFalse(any(d["kind"] == "WorkerPool" for d in objects))


class WorkerDeployTests(unittest.TestCase):
    def deploy(self):
        cluster = mock.Mock(source="/substrate")
        cluster.shell.env = {"PATH": "/bin"}
        return server.Deploy(cluster, get=lambda key: "value" if key in
                             server.SERVER_KEYS + server.ADMIN_KEYS + server.CLIPROXY_KEYS else "")

    def test_worker_uses_pinned_ko_from_source_with_standard_caches(self):
        deploy = self.deploy()
        shell = mock.Mock()
        shell.env = {"PATH": "/bin"}
        shell.output.side_effect = [(0, "amd64\n", ""), (0, "v1-dirty\n", ""),
                                    (0, IMAGE + "\n", "")]
        with mock.patch.object(local, "Shell", return_value=shell) as factory:
            self.assertEqual(deploy._worker_image(), IMAGE)
        env, cwd = factory.call_args.args
        self.assertEqual(cwd, "/substrate")
        self.assertEqual(env["KO_DOCKER_REPO"], server.REGISTRY)
        self.assertEqual(env["KO_DEFAULTPLATFORMS"], "linux/amd64")
        self.assertTrue(env["KO_CACHE"].startswith("/substrate/bin/substrate/"))
        self.assertEqual(shell.output.call_args.args, (
            "/substrate/hack/run-tool.sh", "ko", "build",
            "go.ate.dev/substrate/cmd/ateworker-gvisor", "--base-import-paths",
            "--ldflags=-X=go.ate.dev/substrate/internal/version.Version=v1-dirty"))

    def test_existing_pool_is_never_applied_and_only_alreadyexists_is_ignored(self):
        deploy = self.deploy()
        for code, err, fails in [
            (0, "", False),
            (1, 'Error from server (AlreadyExists): workerpools.ate.dev "brood" already exists',
             False),
            (1, 'Error from server (AlreadyExists): error when creating "/tmp/pool.json": '
             'workerpools.ate.dev "brood" already exists', False),
            (1, 'Error from server (AlreadyExists): workerpools.ate.dev "other" already exists',
             True),
            (1, "Forbidden", True), (1, "connection refused", True)]:
            with self.subTest(err=err), tempfile.TemporaryDirectory() as workdir:
                deploy.shell.output.return_value = (code, "", err)
                if fails:
                    with self.assertRaisesRegex(RuntimeError, re.escape(err)):
                        deploy._bootstrap_pool(workdir, IMAGE)
                else:
                    deploy._bootstrap_pool(workdir, IMAGE)
                args = deploy.shell.output.call_args.args
                self.assertEqual(args[:3], ("kubectl", f"--context={local.CONTEXT}", "create"))
                with open(args[args.index("-f") + 1]) as f:
                    self.assertEqual(json.load(f), server.initial_pool(IMAGE))

    def test_discovery_failures_abort_with_reason(self):
        deploy = self.deploy()
        deploy.shell.output.return_value = (1, "", "Forbidden: cannot get endpoints")
        with self.assertRaisesRegex(RuntimeError, "Kubernetes API discovery.*Forbidden"):
            deploy._api_egress()
        deploy.shell.run.assert_not_called()

    def test_namespace_lookup_failure_does_not_allow_writes(self):
        deploy = self.deploy()
        deploy.shell.output.return_value = (1, "", "Forbidden")
        with self.assertRaisesRegex(RuntimeError, "namespace brood.*Forbidden"):
            deploy._namespace_is_ours()

    def test_namespace_malformed_response_is_explanatory(self):
        deploy = self.deploy()
        for data in ["not json", "[]", '{"metadata": {"labels": []}}',
                     '{"metadata": null}', '{"metadata": {"labels": null}}']:
            with self.subTest(data=data):
                deploy.shell.output.return_value = (0, data, "")
                with self.assertRaisesRegex(RuntimeError, "namespace brood.*invalid JSON"):
                    deploy._namespace_is_ours()

    def test_ownership_failure_stops_before_discovery_build_or_writes(self):
        deploy = self.deploy()
        deploy._tls = mock.Mock(return_value={})
        deploy.cluster.check.side_effect = RuntimeError("foreign container")
        with self.assertRaisesRegex(RuntimeError, "foreign container"):
            deploy.server()
        deploy.shell.run.assert_not_called()
        deploy.shell.output.assert_not_called()

    def test_discovery_reads_both_objects_and_rejects_bad_json(self):
        deploy = self.deploy()
        deploy.shell.output.side_effect = [(0, json.dumps(SERVICE), ""),
                                           (0, json.dumps(ENDPOINTS), "")]
        self.assertEqual(deploy._api_egress(), server.api_egress(SERVICE, ENDPOINTS))
        for call, resource in zip(deploy.shell.output.call_args_list, ["service", "endpoints"]):
            self.assertIn(resource, call.args)
            self.assertIn("default", call.args)
            self.assertIn("--request-timeout=15s", call.args)
        deploy.shell.output.side_effect = [(0, "invalid", "")]
        with self.assertRaisesRegex(RuntimeError, "Kubernetes API discovery.*invalid JSON"):
            deploy._api_egress()

    def test_source_build_must_return_configured_registry_digest(self):
        deploy = self.deploy()
        for code, output, error in [(0, "", ""), (0, "other/worker:latest", ""),
                                    (0, "other/worker@" + DIGEST, ""),
                                    (1, "", "compile failed")]:
            shell = mock.Mock()
            shell.output.side_effect = [(0, "amd64", ""), (0, "v1", ""),
                                        (code, output, error)]
            with self.subTest(output=output, error=error):
                with mock.patch.object(local, "Shell", return_value=shell):
                    with self.assertRaisesRegex(RuntimeError, "worker image"):
                        deploy._worker_image()

    def test_server_integration_order_and_rendered_overlay(self):
        deploy = self.deploy()
        events = []
        deploy.cluster.check.side_effect = lambda: events.append("ownership")
        deploy._namespace_is_ours = lambda: events.append("namespace")
        deploy._api_egress = lambda: events.append("discovery") or server.api_egress(SERVICE, ENDPOINTS)
        deploy._tls = lambda: {"tls.crt": "cert", "tls.key": "key"}
        deploy._image = lambda *_: events.append("build") or DIGEST
        deploy._worker_image = lambda: events.append("worker-build") or IMAGE

        def output(*args):
            self.assertEqual(args[2], "create")
            events.append("pool-create")
            return 0, "created", ""

        def run(*args, **kwargs):
            if "apply" in args:
                events.append("apply")
                workdir = args[-1]
                with open(os.path.join(workdir, "server.env")) as f:
                    self.assertIn(f"WORKER_POOL_IMAGE={IMAGE}\n", f.read())
                with open(os.path.join(workdir, "kustomization.yaml")) as f:
                    overlay = json.load(f)
                all_docs = []
                for resource in overlay["resources"]:
                    if resource == "base":
                        continue
                    with open(os.path.join(workdir, resource)) as f:
                        all_docs.append(json.load(f))
                policy = next(d for d in all_docs if d["kind"] == "NetworkPolicy")
                self.assertEqual(policy["spec"]["egress"], server.api_egress(SERVICE, ENDPOINTS))
                self.assertEqual(policy["metadata"]["namespace"], "brood")
                self.assertEqual(policy["spec"]["podSelector"], {"matchLabels": {"app": "django"}})
                self.assertFalse(any(d["kind"] == "WorkerPool" for d in all_docs))
            if "wait" in args:
                events.append("pool-generated")
                self.assertIn("--for=create", args)
                self.assertIn("deployment/brood", args)
            if "rollout" in args and "deployment/brood" in args:
                events.append("pool-ready")

        deploy.shell.run.side_effect = run
        deploy.shell.output.side_effect = output
        deploy.server()
        self.assertEqual(events[:6], ["ownership", "namespace", "discovery", "build", "build",
                                     "worker-build"])
        self.assertLess(events.index("apply"), events.index("pool-create"))
        self.assertLess(events.index("pool-create"), events.index("pool-generated"))
        self.assertLess(events.index("pool-generated"), events.index("pool-ready"))
