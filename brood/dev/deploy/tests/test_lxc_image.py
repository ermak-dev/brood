"""Образ LXC собирается штатным Dockerfile форка, не distroless ko-образом."""
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

from brood.dev.deploy import infrastructure, lxc
from brood.dev.substrate import local


IMAGE = f"{infrastructure.REGISTRY}/ateworker-lxc@sha256:" + "d" * 64
IDENTITY = "sha256:" + "e" * 64


class LXCImageTests(unittest.TestCase):
    def cluster(self, directory):
        source = Path(directory)
        runtime = source / "cmd" / "ateworker-lxc"
        runtime.mkdir(parents=True)
        for name in ("build.sh", "Dockerfile", "sandboxconfig.yaml"):
            (runtime / name).write_text("fixture")
        return SimpleNamespace(source=str(source), shell=SimpleNamespace(env={"PATH": "/bin"}), check=Mock())

    def test_builds_and_pushes_owned_tag_then_returns_only_registry_digest(self):
        with tempfile.TemporaryDirectory() as directory:
            cluster = self.cluster(directory)
            shell = Mock()
            shell.output.side_effect = [(0, "amd64", ""), (0, "", ""),
                                        (0, IDENTITY, ""), (0, json.dumps([IMAGE]), ""),
                                        (0, IDENTITY, "")]
            with patch.object(local, "Shell", return_value=shell) as factory:
                self.assertEqual(lxc.build_image(cluster), IMAGE)
            self.assertEqual(factory.call_args.args[1], cluster.source)
            calls = [call.args for call in shell.run.call_args_list]
            self.assertEqual(calls[0][0], str(Path(directory) / "cmd/ateworker-lxc/build.sh"))
            tag = calls[0][1]
            self.assertRegex(tag, r"^localhost:5002/ateworker-lxc:brood-[0-9a-f]{32}$")
            self.assertEqual(calls[1:], [("docker", "push", tag), ("docker", "image", "rm", tag)])
            self.assertNotIn("ko", str(shell.mock_calls))

    def test_never_removes_a_temporary_tag_that_changed_identity(self):
        with tempfile.TemporaryDirectory() as directory:
            cluster = self.cluster(directory)
            shell = Mock()
            shell.output.side_effect = [(0, "amd64", ""), (0, "", ""),
                                        (0, IDENTITY, ""), (0, json.dumps([IMAGE]), ""),
                                        (0, "sha256:" + "f" * 64, "")]
            with patch.object(local, "Shell", return_value=shell):
                with self.assertRaisesRegex(RuntimeError, "tag.*changed"):
                    lxc.build_image(cluster)
            self.assertFalse(any(call.args[:3] == ("docker", "image", "rm")
                                 for call in shell.run.call_args_list))

    def test_registry_ownership_change_prevents_push(self):
        with tempfile.TemporaryDirectory() as directory:
            cluster = self.cluster(directory)
            cluster.check.side_effect = RuntimeError("registry ownership changed")
            shell = Mock()
            shell.output.side_effect = [(0, "amd64", ""), (0, "", ""), (0, IDENTITY, ""),
                                        (0, json.dumps([IMAGE]), ""), (0, IDENTITY, "")]
            with patch.object(local, "Shell", return_value=shell):
                with self.assertRaisesRegex(RuntimeError, "registry ownership changed"):
                    lxc.build_image(cluster)
            self.assertFalse(any(call.args[:2] == ("docker", "push")
                                 for call in shell.run.call_args_list))

    def test_unsupported_architecture_or_existing_tag_refuses_before_build(self):
        with tempfile.TemporaryDirectory() as directory:
            cluster = self.cluster(directory)
            for responses in [[(0, "riscv64", "")],
                              [(0, "amd64", ""), (0, "existing-tag", "")]]:
                shell = Mock()
                shell.output.side_effect = responses
                with patch.object(local, "Shell", return_value=shell), self.assertRaises(RuntimeError):
                    lxc.build_image(cluster)
                shell.run.assert_not_called()
