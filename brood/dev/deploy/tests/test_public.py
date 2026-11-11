"""Публичная Go-выкладка начинает с чистой БД и не затрагивает акторов."""
import importlib
import io
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

    def test_short_admin_password_is_accepted(self):
        # Длину пароля администратора выбирает оператор (#222).
        values = self.module().settings(dict(VALUES, BROOD_ADMIN_PASSWORD="nine-char").get)
        self.assertEqual(values["BROOD_ADMIN_PASSWORD"], "nine-char")

    def test_missing_or_multiline_admin_secret_is_explained_without_its_value(self):
        for value in (None, "", "secret-value\nsecond-line", "secret-value\0"):
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

    # FEATURE: Preflight выкладки проверяет BROOD_GIT_REMOTES до сборки edge и обращений к кластеру.
    def test_server_preflight_refuses_invalid_git_remotes_without_values(self):
        command = importlib.import_module("brood.dev.deploy.cli.server")
        token = "synthetic-preflight-token"
        valid = json.dumps({"https://git.example.test/group/repo": {"username": "oauth2", "token": token}})
        self.assertEqual(self.module().settings({**VALUES, "BROOD_GIT_REMOTES": valid}.get)["BROOD_PUBLIC_HOST"],
                         "brood.example.test")
        for raw in (valid.replace(token, token + " x"), valid.replace("https://git", "https://user@git"), "[]"):
            values = {**VALUES, "BROOD_GIT_REMOTES": raw}
            with self.subTest(raw=raw):
                with patch.object(command.config, "get", side_effect=lambda key: values.get(key, "")), \
                        patch.object(command, "Deployment") as deploy, patch.object(command, "cluster") as cluster, \
                        patch("sys.stdout", new_callable=io.StringIO) as out, \
                        self.assertRaises(RuntimeError) as raised:
                    command.main(["--check"])
                self.assertIn("BROOD_GIT_REMOTES", str(raised.exception))
                self.assertNotIn(token, str(raised.exception))
                self.assertNotIn("example.test", str(raised.exception))
                self.assertEqual(out.getvalue(), "")
                deploy.assert_not_called()
                cluster.assert_not_called()

    def test_server_command_preflight_runs_no_mutation(self):
        command = importlib.import_module("brood.dev.deploy.cli.server")
        with patch.object(command, "Deployment", create=True) as deploy, \
                patch.object(command, "cluster", create=True), \
                patch.object(command, "settings", return_value=VALUES), \
                patch("sys.stdout", new_callable=io.StringIO):
            deploy.return_value.space.return_value = {"checked": False}
            command.main(["--check"])
            deploy.return_value.space.assert_called_once()
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


# FEATURE: Disk guard of the deploy machine (#247).
# SPEC: free space of the Docker partition is checked before the Lease, the
# build and any write; after a successful deploy old own image tags are removed
# and the build cache is pruned; unset settings keep the previous behaviour and
# the command says so; a failed deploy cleans nothing.
class DiskGuardTests(unittest.TestCase):
    SERVER_TAG = "localhost:5002/brood-go:build-" + "a" * 32
    DIGEST = "sha256:" + "e" * 64

    def module(self):
        return importlib.import_module("brood.dev.deploy.public")

    def shell(self, available_kib=40 << 20, events=None):
        from brood.dev.deploy.tests.test_disk import FakeShell, df, image
        events = [] if events is None else events
        built = {}

        def buildx(argv):
            events.append("build-edge")
            built["edge"] = argv[argv.index("-t") + 1]
            Path(argv[argv.index("--metadata-file") + 1]).write_text(json.dumps({"containerimage.digest": self.DIGEST}))
            return 0, "", ""

        def listing(argv):
            events.append("image-ls")
            return 0, "\n".join([
                image("localhost:5002/brood-go", self.SERVER_TAG.rpartition(":")[2], "id-new", "2026-11-11 10:00:00 +0000 UTC"),
                image("localhost:5002/brood-go", "build-" + "b" * 32, "id-prev", "2026-11-10 10:00:00 +0000 UTC"),
                image("localhost:5002/brood-go", "build-" + "c" * 32, "id-old", "2026-11-09 10:00:00 +0000 UTC"),
                image("localhost:5002/brood-go-edge", built["edge"].rpartition(":")[2], "id-edge",
                      "2026-11-01 10:00:00 +0000 UTC")]), ""

        def record(name, answer):
            def call(argv):
                events.append(name)
                return answer
            return call

        return FakeShell({("docker", "info"): record("space", (0, "/var/lib/docker\n", "")),
                          ("df",): (0, df(available_kib), ""),
                          ("docker", "system", "df"): (0, "", ""),
                          ("docker", "buildx", "build"): buildx,
                          ("docker", "image", "ls"): listing,
                          ("docker", "image", "rm"): record("image-rm", (0, "", "")),
                          ("docker", "builder", "prune"): record("prune", (0, "Total:\t55.2GB\n", ""))})

    def deployment(self, values, shell, events):
        module = self.module()
        deploy = module.Deployment(Mock(shell=shell), get=values.get, project="/release")
        deploy.check = Mock(side_effect=lambda *a: events.append("check") or (values, {}))
        deploy.bootstrap = Mock(return_value=False)
        deploy.apply_edge = Mock(side_effect=lambda *a: events.append("edge"))
        deploy.probe = Mock()
        deploy.cleanup = Mock(side_effect=lambda before: events.append("legacy-cleanup"))
        return module, deploy

    def run_initial(self, values, available_kib=40 << 20, fail=None):
        events = []
        shell = self.shell(available_kib, events)
        module, deploy = self.deployment(values, shell, events)
        if fail:
            deploy.probe.side_effect = RuntimeError(fail)
        with patch.object(module, "GoDeploy") as go:
            go.return_value.tag = self.SERVER_TAG
            go.return_value.run.side_effect = lambda **k: events.append("server") or "localhost:5002/brood-go@" + self.DIGEST
            try:
                return deploy.run(), events, shell
            except RuntimeError as error:
                return error, events, shell

    def test_low_free_space_refuses_before_any_build_or_write(self):
        result, events, _ = self.run_initial({**VALUES, "BROOD_DEPLOY_MIN_FREE_GB": "10",
                                              "BROOD_DEPLOY_BUILD_CACHE_GB": "5"}, available_kib=3 << 20)
        self.assertIsInstance(result, RuntimeError)
        self.assertRegex(str(result), "3.0 GiB.*BROOD_DEPLOY_MIN_FREE_GB=10")
        self.assertEqual(events, ["space"])

    def test_existing_mode_checks_space_before_the_lease(self):
        module = self.module()
        events = []
        values = {**VALUES, "BROOD_DEPLOY_MODE": "existing", "BROOD_DEPLOY_MIN_FREE_GB": "10"}
        _, deploy = self.deployment(values, self.shell(1 << 20, events), events)
        with patch.object(module, "ExistingInstall") as existing, patch.object(module, "GoDeploy") as go:
            with self.assertRaisesRegex(RuntimeError, "MIN_FREE_GB"):
                deploy.run()
            existing.return_value.claim.assert_not_called()
            go.assert_not_called()
        self.assertEqual(events, ["space"])

    def test_successful_deploy_removes_old_own_tags_then_prunes_the_cache(self):
        result, events, shell = self.run_initial({**VALUES, "BROOD_DEPLOY_MIN_FREE_GB": "10",
                                                  "BROOD_DEPLOY_BUILD_CACHE_GB": "5"})
        self.assertEqual(events, ["space", "check", "build-edge", "server", "edge", "legacy-cleanup",
                                  "image-ls", "image-rm", "prune"])
        self.assertEqual(result["space"]["free"], 40 << 30)
        self.assertEqual(result["docker"]["removed"], ["localhost:5002/brood-go:build-" + "c" * 32])
        # Тег edge этой выкладки — тот, что собрал build_edge, а не первый попавшийся.
        built = next(c[c.index("-t") + 1] for c in shell.calls if "buildx" in c)
        self.assertTrue(built.startswith("localhost:5002/brood-go-edge:build-"))
        self.assertEqual(result["docker"]["kept"], [self.SERVER_TAG, "localhost:5002/brood-go:build-" + "b" * 32,
                                                    built])
        self.assertEqual(result["docker"]["reclaimed"], "55.2GB")
        self.assertNotIn("tags", result)

    def test_failed_deploy_cleans_nothing(self):
        result, events, _ = self.run_initial({**VALUES, "BROOD_DEPLOY_BUILD_CACHE_GB": "5"}, fail="probe failed")
        self.assertRegex(str(result), "probe failed")
        self.assertNotIn("image-rm", events)
        self.assertNotIn("prune", events)

    def test_unset_settings_keep_the_previous_deploy(self):
        result, events, shell = self.run_initial(VALUES)
        self.assertEqual(events, ["check", "build-edge", "server", "edge", "legacy-cleanup"])
        self.assertEqual(result["space"], {"checked": False})
        self.assertEqual(result["docker"], {"enabled": False})

    def test_malformed_disk_settings_are_refused_before_cluster_access(self):
        command = importlib.import_module("brood.dev.deploy.cli.server")
        for name in ("BROOD_DEPLOY_MIN_FREE_GB", "BROOD_DEPLOY_BUILD_CACHE_GB", "BROOD_DEPLOY_KEEP_SNAPSHOTS"):
            values = {**VALUES, name: "10GB"}
            with self.subTest(name=name), \
                    patch.object(command.config, "get", side_effect=lambda key: values.get(key, "")), \
                    patch.object(command, "Deployment") as deploy, patch.object(command, "cluster") as cluster, \
                    self.assertRaisesRegex(RuntimeError, name):
                command.main(["--check"])
            deploy.assert_not_called()
            cluster.assert_not_called()

    def printed(self, argv, **mocked):
        command = importlib.import_module("brood.dev.deploy.cli.server")
        with patch.object(command, "settings"), patch.object(command, "cluster"), \
                patch.object(command, "Deployment") as deploy, patch("sys.stdout", new_callable=io.StringIO) as out:
            for name, value in mocked.items():
                getattr(deploy.return_value, name).return_value = value
            command.main(argv)
        return out.getvalue()

    def test_check_reports_measured_space_or_warns_that_it_is_skipped(self):
        out = self.printed(["--check"], space={"checked": True, "mount": "/", "root": "/var/lib/docker",
                                               "free": 42 << 30, "limit": 10 << 30})
        self.assertIn("Free space: 42.0 GiB on / (Docker root /var/lib/docker), required 10.0 GiB", out)
        out = self.printed(["--check"], space={"checked": False})
        self.assertIn("warning: free space check skipped: BROOD_DEPLOY_MIN_FREE_GB is not set", out)
        self.assertIn("Public Go deployment preflight passed", out)

    def test_run_reports_cleanup_results_and_failures_as_warnings(self):
        base = {"image": "img", "host": "h.example", "admin": "admin", "space": {"checked": False}}
        out = self.printed([], run={**base, "docker": {"enabled": False}})
        self.assertIn("warning: Docker cleanup skipped: BROOD_DEPLOY_BUILD_CACHE_GB is not set", out)
        docker = {"enabled": True, "cache": 5, "removed": ["r1", "r2"], "kept": ["k1", "k2"],
                  "reclaimed": "55.2GB", "errors": []}
        out = self.printed([], run={**base, "docker": docker})
        self.assertIn("Docker cleanup: removed 2 old release image tags, kept k1, k2; "
                      "build cache pruned to 5 GiB, reclaimed 55.2GB", out)
        out = self.printed([], run={**base, "docker": {**docker, "errors": ["build cache: boom"]}})
        self.assertIn("warning: Docker cleanup failed: build cache: boom", out)

    def test_check_refuses_on_space_before_the_install_preflight(self):
        command = importlib.import_module("brood.dev.deploy.cli.server")
        with patch.object(command, "settings"), patch.object(command, "cluster"), \
                patch.object(command, "Deployment") as deploy, patch("sys.stdout", new_callable=io.StringIO) as out:
            deploy.return_value.space.side_effect = RuntimeError("free space on / (Docker root) is 1.0 GiB")
            with self.assertRaisesRegex(RuntimeError, "free space"):
                command.main(["--check"])
        deploy.return_value.check.assert_not_called()
        self.assertEqual(out.getvalue(), "")
