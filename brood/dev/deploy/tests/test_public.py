"""Публичная Go-выкладка начинает с чистой БД и не затрагивает акторов."""
import importlib
import json
from pathlib import Path
import unittest
from unittest.mock import Mock, patch


VALUES = {"BROOD_SESSION_SECRET": "s" * 64, "BROOD_ADMIN_USERNAME": "admin",
          "BROOD_ADMIN_PASSWORD": "new-random-password", "BROOD_PUBLIC_HOST": "brood.example.test",
          "BROOD_HTTPS_PORT": "443", "CLIPROXY_URL": "http://cliproxy:8317",
          "CLIPROXY_MANAGEMENT_KEY": "server-only-key"}


class PublicSettingsTests(unittest.TestCase):
    def module(self):
        return importlib.import_module("brood.dev.deploy.public")

    def test_fresh_settings_do_not_require_any_legacy_migration(self):
        values = self.module().settings(VALUES.get)
        self.assertEqual(values["BROOD_ADMIN_USERNAME"], "admin")
        self.assertEqual(values["BROOD_PUBLIC_HOST"], "brood.example.test")
        self.assertNotIn("DJANGO_SECRET_KEY", values)
        self.assertNotIn("POSTGRES_PASSWORD", values)

    def test_missing_or_multiline_admin_secret_is_explained_without_its_value(self):
        for value in (None, "", "too-short", "secret-value\nsecond-line"):
            values = dict(VALUES, BROOD_ADMIN_PASSWORD=value)
            with self.subTest(value=value), self.assertRaises(RuntimeError) as error:
                self.module().settings(values.get)
            self.assertIn("BROOD_ADMIN_PASSWORD", str(error.exception))
            self.assertNotIn("secret-value", str(error.exception))

    def test_legacy_edge_identity_is_validated_before_adoption(self):
        obj = {"kind": "Deployment", "metadata": {"name": "edge", "namespace": "brood", "uid": "known"},
               "spec": {"selector": {"matchLabels": {"app": "edge"}}}}
        self.assertEqual(self.module().identity(obj, "deployment", "edge"), "known")
        obj["spec"]["selector"]["matchLabels"] = {"app": "someone-else"}
        with self.assertRaisesRegex(RuntimeError, "foreign"):
            self.module().identity(obj, "deployment", "edge")

    def test_declared_foreign_owner_cannot_be_adopted_by_matching_selector(self):
        obj = {"metadata": {"name": "edge", "namespace": "brood", "uid": "foreign",
                            "labels": {"app.kubernetes.io/managed-by": "other-installer"}},
               "spec": {"selector": {"matchLabels": {"app": "edge"}}}}
        with self.assertRaisesRegex(RuntimeError, "foreign"):
            self.module().identity(obj, "deployment", "edge")

    def test_admin_bootstrap_uses_stdin_and_never_password_arguments(self):
        module = self.module()
        cluster = Mock(kubectl="kubectl", shell=Mock(env={}))
        runner = Mock(return_value=Mock(returncode=0))
        deploy = module.Deployment(cluster, get=VALUES.get, runner=runner)
        deploy.bootstrap(VALUES)
        argv = runner.call_args.args[0]
        self.assertIn("bootstrap-admin", argv)
        self.assertNotIn(VALUES["BROOD_ADMIN_PASSWORD"], argv)
        self.assertEqual(runner.call_args.kwargs["input"], "admin\nnew-random-password\n")

    def test_server_command_preflight_runs_no_mutation(self):
        command = importlib.import_module("brood.dev.deploy.cli.server")
        with patch.object(command, "Deployment", create=True) as deploy, \
                patch.object(command, "cluster", create=True), \
                patch.object(command, "settings", return_value=VALUES):
            command.main(["--check"])
            deploy.return_value.check.assert_called_once()
            deploy.return_value.run.assert_not_called()

    def test_legacy_cleanup_allowlist_excludes_shared_resources(self):
        names = self.module().LEGACY
        self.assertIn(("deployment", "django"), names)
        self.assertIn(("pvc", "data-postgres-0"), names)
        for kind, name in names:
            self.assertNotIn(kind, {"namespace", "workerpool", "actor"})
            self.assertNotIn(name, {"brood-development", "brood-microvm", "cliproxy-data"})

    def test_reset_rollout_does_not_invoke_import_tool(self):
        root = Path(__file__).resolve().parents[4]
        source = (root / "brood/dev/deploy/public.py").read_text()
        self.assertNotIn("brood-migrate", source)
        self.assertNotIn("substrate uninstall", source)
        self.assertNotIn("delete namespace", source)

    def test_go_management_configuration_uses_secret_references_and_scoped_egress(self):
        from brood.dev.deploy.go_server import manifests
        objects = manifests("localhost:5002/brood-go@sha256:" + "a" * 64,
                            {"gvisor": "registry/worker@sha256:" + "b" * 64}, proxy=True)
        pod = next(obj for obj in objects if obj["kind"] == "Deployment")
        env = {item["name"]: item for item in pod["spec"]["template"]["spec"]["containers"][0]["env"]}
        for key in ("CLIPROXY_URL", "CLIPROXY_MANAGEMENT_KEY"):
            self.assertNotIn("value", env[key])
            self.assertEqual(env[key]["valueFrom"]["secretKeyRef"]["name"], "brood-go-env")
        policy = next(obj for obj in objects if obj["kind"] == "NetworkPolicy")
        self.assertTrue(any(rule.get("to") == [{"podSelector": {"matchLabels": {"app": "cliproxy"}}}]
                            for rule in policy["spec"]["egress"]))

    def test_retry_with_existing_admin_cannot_skip_authenticated_gate(self):
        module = self.module()
        deploy = module.Deployment(Mock(), get=VALUES.get)
        deploy.check = Mock(return_value=(VALUES, {("pvc", "data-postgres-0"): "old-db"}))
        deploy.build_edge = Mock(return_value="edge-image")
        deploy.bootstrap = Mock(return_value=False)
        deploy.apply_edge = Mock()
        deploy.cleanup = Mock()

        def probe(values, login):
            if login:
                raise RuntimeError("admin verification failed")

        deploy.probe = Mock(side_effect=probe)
        with patch.object(module, "GoDeploy"):
            with self.assertRaisesRegex(RuntimeError, "admin verification failed"):
                deploy.run()
        deploy.cleanup.assert_not_called()

    # BUG: apply сохраняет старый nginx рядом с новым контейнером edge.
    # HYPOTHESIS: replace удаляет поля, отсутствующие в прежнем apply ledger.
    # RESULT: FIXED — replace с UID/resourceVersion; отсутствующие ресурсы создаются.
    def test_existing_edge_is_replaced_without_merging_legacy_containers(self):
        module = self.module()
        cluster = Mock(shell=Mock())
        deployment = module.Deployment(cluster, get=VALUES.get)
        current = {"metadata": {"name": "edge", "namespace": "brood", "uid": "edge-uid",
                                "resourceVersion": "42"},
                   "spec": {"selector": {"matchLabels": {"app": "edge"}},
                            "template": {"spec": {"containers": [{"name": "nginx"}]}}}}
        deployment.get_object = Mock(side_effect=lambda kind, name: current if kind == "deployment" else None)
        operations = []

        def output(*args):
            if "-f" in args:
                document = json.loads(Path(args[args.index("-f") + 1]).read_text())
                objects = document.get("items", [document])
                for obj in objects:
                    operations.append((obj["kind"], "replace" if "replace" in args else "create"))
                    if obj["kind"] == "Deployment":
                        self.assertIn("replace", args)
                        self.assertNotIn("--force", args)
                        self.assertEqual(obj["metadata"]["resourceVersion"], "42")
                        self.assertEqual(obj["metadata"]["uid"], "edge-uid")
                        self.assertEqual([c["name"] for c in obj["spec"]["template"]["spec"]["containers"]], ["edge"])
            return 0, "", ""

        cluster.shell.output.side_effect = output
        deployment.apply_edge("localhost:5002/brood-go-edge@sha256:" + "a" * 64,
                              {("deployment", "edge"): "edge-uid"})
        self.assertIn(("Deployment", "replace"), operations)
        self.assertIn(("Service", "create"), operations)

    def test_uid_change_refuses_cleanup(self):
        module = self.module()
        cluster = Mock(shell=Mock())
        changed = {"kind": "Deployment", "metadata": {"name": "django", "namespace": "brood", "uid": "new"},
                   "spec": {"selector": {"matchLabels": {"app": "django"}}}}
        cluster.shell.output.return_value = (0, json.dumps(changed), "")
        deploy = module.Deployment(cluster, get=VALUES.get)
        with self.assertRaisesRegex(RuntimeError, "identity changed"):
            deploy.cleanup({("deployment", "django"): "old"})
        self.assertEqual(cluster.shell.output.call_count, 1)
