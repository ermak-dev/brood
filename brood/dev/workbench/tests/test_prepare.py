"""Подготовка приватного Go endpoint разрешена только внутри гостя."""
import importlib
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock


IMAGE = "localhost:5002/ateworker-gvisor@sha256:" + "a" * 64


class GuestPreparationTests(unittest.TestCase):
    def setUp(self):
        self.module = importlib.import_module("brood.dev.workbench.prepare")
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        (self.root / "brood").mkdir()
        (self.root / "brood/.env").write_text("BROOD_SESSION_SECRET=fresh-secret\n")
        self.env = {"BROOD_WORKBENCH": "1", "DOCKER_HOST": "unix:///var/run/docker.sock"}
        self.cluster = mock.Mock(source=str(self.root / "substrate"))
        self.cluster.shell.output.return_value = (0, "", "")

    def prepare(self):
        return self.module.prepare(self.cluster, self.root, self.env)

    def test_host_or_external_source_is_rejected_before_cluster_calls(self):
        for env, source in (({}, self.root / "substrate"),
                            (self.env, self.root / "operator-substrate")):
            self.cluster.source = str(source)
            with self.subTest(env=env, source=source), self.assertRaisesRegex(RuntimeError, "workbench|guest"):
                self.module.prepare(self.cluster, self.root, env)
            self.cluster.check.assert_not_called()
            self.cluster.shell.output.assert_not_called()

    def test_foreign_namespace_is_rejected_before_build_or_apply(self):
        self.cluster.shell.output.return_value = (0, json.dumps({"metadata": {"labels": {}}}), "")
        with mock.patch.object(self.module, "Infrastructure") as infrastructure:
            with self.assertRaisesRegex(RuntimeError, "foreign.*namespace"):
                self.prepare()
            infrastructure.assert_not_called()
        self.assertFalse(any("apply" in call.args for call in self.cluster.shell.output.call_args_list))

    def test_foreign_service_account_is_rejected_before_any_mutation(self):
        owned = {"metadata": {"labels": self.module.OWNER}}
        self.cluster.shell.output.side_effect = [(0, json.dumps(owned), ""),
                                                 (0, json.dumps({"metadata": {}}), "")]
        with mock.patch.object(self.module, "Infrastructure") as infrastructure:
            with self.assertRaisesRegex(RuntimeError, "foreign.*ServiceAccount"):
                self.prepare()
            infrastructure.assert_not_called()

    def test_fresh_guest_gets_owned_infrastructure_and_approved_worker_setting(self):
        captured = []

        def output(*args):
            if "create" in args:
                captured.append(json.loads(Path(args[args.index("-f") + 1]).read_text()))
            return 0, "", ""

        self.cluster.shell.output.side_effect = output
        with mock.patch.object(self.module, "Infrastructure") as infrastructure:
            infrastructure.return_value._worker_image.return_value = IMAGE
            self.assertEqual(self.prepare(), {"gvisor": IMAGE})
        self.cluster.check.assert_called_once()
        infrastructure.return_value._worker_image.assert_called_once_with("gvisor")
        objects = captured
        self.assertEqual([obj["kind"] for obj in objects],
                         ["Namespace", "ServiceAccount", "Role", "RoleBinding"])
        for obj in objects:
            expected = dict(self.module.OWNER)
            if obj["kind"] == "Namespace":
                expected["ate.dev/api-access"] = "true"
            self.assertEqual(obj["metadata"]["labels"], expected)
        self.assertNotIn("Ingress", str(objects))
        text = (self.root / "brood/.env").read_text()
        self.assertIn("BROOD_SESSION_SECRET=fresh-secret\n", text)
        self.assertIn('WORKER_POOL_IMAGES={"gvisor": "' + IMAGE + '"}', text)
        self.assertEqual((self.root / "brood/.env").stat().st_mode & 0o777, 0o600)

    def test_namespace_created_after_inspection_is_not_overwritten(self):
        def output(*args):
            return (1, "", "AlreadyExists") if "create" in args else (0, "", "")

        self.cluster.shell.output.side_effect = output
        with mock.patch.object(self.module, "Infrastructure") as infrastructure:
            infrastructure.return_value._worker_image.return_value = IMAGE
            with self.assertRaisesRegex(RuntimeError, "create.*Namespace"):
                self.prepare()
        mutations = [call.args for call in self.cluster.shell.output.call_args_list
                     if "create" in call.args or "apply" in call.args]
        self.assertEqual(len(mutations), 1)
        self.assertIn("create", mutations[0])
        self.assertNotIn("WORKER_POOL_IMAGES", (self.root / "brood/.env").read_text())

    def test_cli_rejects_host_before_constructing_cluster(self):
        command = importlib.import_module("brood.dev.workbench.cli.prepare")
        with mock.patch.dict(command.os.environ, {}, clear=True), \
                mock.patch.object(command, "cluster") as cluster:
            with self.assertRaisesRegex(RuntimeError, "workbench"):
                command.main([])
            cluster.assert_not_called()

    def test_missing_guest_settings_is_refused_before_cluster_calls(self):
        (self.root / "brood/.env").unlink()
        with self.assertRaisesRegex(RuntimeError, "guest.*settings"):
            self.prepare()
        self.cluster.check.assert_not_called()
