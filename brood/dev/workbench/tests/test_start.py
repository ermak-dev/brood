"""Выбранный workspace должен совпадать у Docker, задания и кэшей."""
import os
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from brood.dev.workbench.cli import start


class StartCommandTests(unittest.TestCase):
    def test_docker_failure_is_reported_while_results_remain_available(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "logs").mkdir()
            events = []
            def unavailable(_):
                events.append("docker")
                raise RuntimeError("Docker bootstrap failed")
            with mock.patch.dict(os.environ, {
                    "BROOD_WORKBENCH": "1", "DOCKER_HOST": "unix:///var/run/docker.sock",
                    "WORKBENCH_ROOT": str(root), "WORKBENCH_SEEDS": "/opt/seeds"}, clear=True), \
                    mock.patch.object(start.runtime, "prepare_workspace", return_value=root / "brood"), \
                    mock.patch.object(start.runtime, "write_status") as status, \
                    mock.patch.object(start.runtime, "prepare_kernel_log_device",
                                      side_effect=lambda: events.append("device")), \
                    mock.patch("brood.dev.workbench.storage.prepare",
                               side_effect=lambda _: events.append("storage")), \
                    mock.patch.object(start.runtime, "start_docker", side_effect=unavailable), \
                    mock.patch.object(start.subprocess, "Popen", side_effect=lambda *a, **k: events.append("http")), \
                    mock.patch.object(start.os, "execvp") as execute, mock.patch("builtins.print"):
                start.main([])
                self.assertEqual(events, ["http", "device", "storage", "docker"])
                status.assert_called_with(root, "failed", "Docker bootstrap failed")
                execute.assert_called_once_with("sleep", ["sleep", "infinity"])

    def test_selected_workspace_is_propagated_to_the_job_and_caches(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp) / "selected"
            (root / "logs").mkdir(parents=True)
            project = root / "brood"
            with mock.patch.dict(os.environ, {
                    "BROOD_WORKBENCH": "1", "DOCKER_HOST": "unix:///var/run/docker.sock",
                    "WORKBENCH_ROOT": "/workspace", "WORKBENCH_SEEDS": "/opt/seeds"}, clear=True), \
                    mock.patch.object(start.runtime, "prepare_workspace", return_value=project), \
                    mock.patch.object(start.runtime, "write_status"), \
                    mock.patch.object(start.runtime, "prepare_kernel_log_device"), \
                    mock.patch("brood.dev.workbench.storage.prepare"), \
                    mock.patch.object(start.runtime, "start_docker"), \
                    mock.patch.object(start.subprocess, "Popen"), \
                    mock.patch.object(start.os, "chdir"), \
                    mock.patch.object(start.os, "execvp") as execute, mock.patch("builtins.print"):
                start.main(["--root", str(root)])
                self.assertEqual(os.environ["WORKBENCH_ROOT"], str(root))
                self.assertEqual(os.environ["HOME"], str(root / "home"))
                self.assertEqual(os.environ["TMPDIR"], str(root / "tmp"))
                self.assertEqual(os.environ["XDG_DATA_HOME"], str(root / "data"))
                self.assertEqual(os.environ["SUBSTRATE_SOURCE"], str(root / "substrate"))
                self.assertEqual(os.environ["GOCACHE"], str(root / "cache/go-build"))
                self.assertEqual(os.environ["GOMODCACHE"], str(root / "cache/go-mod"))
                execute.assert_called_once_with("bash", ["bash", str(project / ".ci/workbench.sh")])
