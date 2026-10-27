"""Рабочее дерево и Docker внутри гостя не используют настройки стенда."""
import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from brood.common import config
from brood.dev.workbench import runtime


class WorkspaceTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.base = Path(self.tmp.name)
        self.root, self.seeds = self.base / "workspace", self.base / "seeds"
        for name, marker in (("brood", "bin/brood"), ("substrate", "hack/install-ate-kind.sh")):
            path = self.seeds / name / marker
            path.parent.mkdir(parents=True)
            path.write_text("seed")
        self.generated = []

    def token(self):
        value = f"new-secret-{len(self.generated)}"
        self.generated.append(value)
        return value

    def openssl(self, args, **kwargs):
        self.assertEqual(args[:2], ["openssl", "req"])
        for option in ("-keyout", "-out"):
            Path(args[args.index(option) + 1]).write_text("test certificate")
        return mock.Mock(returncode=0, stdout="", stderr="")

    def test_settings_are_fresh_local_and_do_not_copy_production_credentials(self):
        values = runtime.development_settings(self.root, self.token)
        # FEATURE: Изолированный workbench принимает только Go runtime.
        # SPEC: Свежий session secret; без Django, Postgres и proxy credentials.
        # IMPLEMENTATION: Go session secret и гостевой TLS создаются один раз.
        # RESULT: IMPLEMENTED — private settings и TLS сохраняются при повторном старте.
        # STATUS: DONE
        self.assertIn(values["BROOD_SESSION_SECRET"], self.generated)
        self.assertEqual(len(set(self.generated)), 1)
        self.assertFalse(any(key.startswith(("DJANGO_", "POSTGRES_", "CLIPROXY_"))
                             for key in values))
        self.assertEqual(values["SUBSTRATE_SOURCE"], str(self.root / "substrate"))
        self.assertEqual(values["BROOD_HTTP_PORT"], "8080")
        self.assertEqual(values["BROOD_HTTPS_PORT"], "8443")
        self.assertFalse(set(values) & {"GITLAB_TOKEN", "KUBE_API_TOKEN", "ATEAPI_TOKEN",
                                       "BROOD_DEPLOY_SSH_KEY", "BROOD_DEPLOY_HOST"})

    def test_workspace_copies_seeds_and_creates_private_settings_once(self):
        runtime.prepare_workspace(self.root, self.seeds, self.token, self.openssl)
        project = self.root / "brood"
        env = project / ".env"
        before = env.read_text()
        self.assertEqual(env.stat().st_mode & 0o777, 0o600)
        self.assertEqual(config.parse_env(before)["BROOD_SESSION_SECRET"], "new-secret-0")
        self.assertEqual((project / "bin/brood").read_text(), "seed")
        for directory in ("docker", "tmp", "home", "data", "logs", "results"):
            self.assertTrue((self.root / directory).is_dir())
        for name in ("tls.crt", "tls.key"):
            self.assertEqual((project / "secrets" / name).stat().st_mode & 0o777, 0o600)
        (project / "bin/brood").write_text("operator changes")
        runtime.prepare_workspace(self.root, self.seeds, self.token, self.openssl)
        self.assertEqual(env.read_text(), before)
        self.assertEqual((project / "bin/brood").read_text(), "operator changes")
        self.assertEqual(len(self.generated), 1)

    def test_seed_with_env_is_rejected_before_copying_credentials(self):
        (self.seeds / "brood" / ".env").write_text("GITLAB_TOKEN=private")
        with self.assertRaisesRegex(RuntimeError, "seed.*.env"):
            runtime.prepare_workspace(self.root, self.seeds, self.token, self.openssl)
        self.assertFalse((self.root / "brood" / ".env").exists())

    def test_partial_tls_is_not_silently_replaced(self):
        runtime.prepare_workspace(self.root, self.seeds, self.token, self.openssl)
        key = self.root / "brood/secrets/tls.key"
        key.unlink()
        with self.assertRaisesRegex(RuntimeError, "TLS.*incomplete"):
            runtime.prepare_workspace(self.root, self.seeds, self.token, self.openssl)
        self.assertFalse(key.exists())

    def test_image_marker_and_local_docker_socket_are_required(self):
        for env in ({}, {"BROOD_WORKBENCH": "1", "DOCKER_HOST": "tcp://production:2375"}):
            with self.subTest(env=env), self.assertRaisesRegex(RuntimeError, "workbench|DOCKER_HOST"):
                runtime.check_environment(env)
        self.assertIsNone(runtime.check_environment({
            "BROOD_WORKBENCH": "1", "DOCKER_HOST": "unix:///var/run/docker.sock"}))

    def test_daemon_command_prepares_nested_cgroup_controllers(self):
        self.assertEqual(runtime.daemon_command(self.root)[:2], ["dind", "dockerd"])

    def test_daemon_command_uses_vfs_and_workspace_storage(self):
        args = runtime.daemon_command(self.root)
        self.assertEqual(args[:2], ["dind", "dockerd"])
        self.assertIn("--host=unix:///var/run/docker.sock", args)
        self.assertIn(f"--data-root={self.root / 'docker'}", args)
        self.assertIn("--storage-driver=vfs", args)
        self.assertIn("--feature=containerd-snapshotter=false", args)
        self.assertFalse(any("tcp://" in arg or "2375" in arg for arg in args))

    def test_missing_docker_binary_has_a_reason_without_a_traceback(self):
        self.root.mkdir()
        (self.root / "logs").mkdir()
        with mock.patch.object(runtime.os.path, "ismount", return_value=True):
            with self.assertRaisesRegex(RuntimeError, "dockerd.*not installed"):
                runtime.start_docker(self.root, run=mock.Mock(return_value=mock.Mock(
                    returncode=0, stdout="", stderr="")), popen=mock.Mock(side_effect=FileNotFoundError()))

    def test_startup_timeout_terminates_only_the_daemon_it_started(self):
        self.root.mkdir()
        (self.root / "logs").mkdir()
        process = mock.Mock()
        process.poll.return_value = None
        run = mock.Mock(return_value=mock.Mock(returncode=1, stdout="", stderr="not ready"))
        run.side_effect = lambda args, **kwargs: mock.Mock(
            returncode=0 if args[0] == "mount" else 1, stdout="", stderr="not ready")
        with mock.patch.object(runtime.os.path, "ismount", return_value=True):
            with self.assertRaisesRegex(RuntimeError, "did not become ready"):
                runtime.start_docker(self.root, run=run, popen=mock.Mock(return_value=process),
                                     clock=mock.Mock(side_effect=[0, 91]), sleep=mock.Mock())
        process.terminate.assert_called_once()
        process.wait.assert_called_once_with(timeout=15)

    def test_status_is_data_without_credentials(self):
        runtime.write_status(self.root, "starting", "Preparing isolated environment")
        value = json.loads((self.root / "results/status.json").read_text())
        self.assertEqual(value["state"], "starting")
        self.assertEqual(value["message"], "Preparing isolated environment")
        self.assertNotIn("secret", str(value).lower())
        self.assertEqual(os.stat(self.root / "results/status.json").st_mode & 0o777, 0o644)
