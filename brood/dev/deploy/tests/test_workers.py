"""Общие проверки инфраструктуры без старого серверного runtime."""
import json
import unittest
from unittest import mock

from brood.dev.deploy import infrastructure
from brood.dev.substrate import local

DIGEST = "sha256:" + "a" * 64
IMAGE = f"{infrastructure.REGISTRY}/ateworker-gvisor@{DIGEST}"
SERVICE = {"spec": {"clusterIPs": ["10.96.0.1", "fd00::1"],
                    "ports": [{"name": "https", "port": 443, "protocol": "TCP"}]}}
ENDPOINTS = {"subsets": [{"addresses": [{"ip": "172.18.0.2"}],
                          "ports": [{"name": "https", "port": 6443, "protocol": "TCP"}]}]}


class InfrastructureTests(unittest.TestCase):
    def deploy(self):
        cluster = mock.Mock(source="/substrate")
        cluster.shell.env = {"PATH": "/bin"}
        return infrastructure.Infrastructure(cluster)

    def test_exact_api_egress_before_and_after_dnat(self):
        self.assertEqual(infrastructure.api_egress(SERVICE, ENDPOINTS), [
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
                    infrastructure.api_egress(service, endpoints)

    def test_worker_uses_pinned_ko_from_source_with_standard_caches(self):
        deploy = self.deploy()
        shell = mock.Mock()
        shell.output.side_effect = [(0, "amd64\n", ""), (0, "v1-dirty\n", ""),
                                    (0, IMAGE + "\n", "")]
        with mock.patch.object(local, "Shell", return_value=shell) as factory:
            self.assertEqual(deploy._worker_image(), IMAGE)
        env, cwd = factory.call_args.args
        self.assertEqual(cwd, "/substrate")
        self.assertEqual(env["KO_DOCKER_REPO"], infrastructure.REGISTRY)
        self.assertEqual(env["KO_DEFAULTPLATFORMS"], "linux/amd64")
        self.assertTrue(env["KO_CACHE"].startswith("/substrate/bin/substrate/"))
        self.assertEqual(shell.output.call_args.args, (
            "/substrate/hack/run-tool.sh", "ko", "build",
            "go.ate.dev/substrate/cmd/ateworker-gvisor", "--base-import-paths",
            "--ldflags=-X=go.ate.dev/substrate/internal/version.Version=v1-dirty"))

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

    def test_namespace_must_exist_and_be_owned(self):
        deploy = self.deploy()
        for data in ("", '{"metadata":{"labels":{}}}'):
            with self.subTest(data=data), self.assertRaises(RuntimeError):
                deploy.shell.output.return_value = (0, data, "")
                deploy._namespace_is_ours()
        deploy.shell.run.assert_not_called()

    def test_discovery_reads_both_objects_and_rejects_bad_json(self):
        deploy = self.deploy()
        deploy.shell.output.side_effect = [(0, json.dumps(SERVICE), ""),
                                           (0, json.dumps(ENDPOINTS), "")]
        self.assertEqual(deploy._api_egress(), infrastructure.api_egress(SERVICE, ENDPOINTS))
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

    def test_env_file_preserves_values_and_refuses_newlines(self):
        self.assertEqual(infrastructure.env_file({"VALUE": "a=b'c"}), "VALUE=a=b'c\n")
        with self.assertRaisesRegex(RuntimeError, "VALUE.*spans lines"):
            infrastructure.env_file({"VALUE": "a\nb"})
