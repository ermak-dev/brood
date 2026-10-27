"""Образ получает только Git-снимки и закреплённые инструменты проекта."""
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from brood.common import cli
from brood.dev.workbench import build


class ImageBuildTests(unittest.TestCase):
    def test_context_delegates_to_the_generic_committed_source_exporter(self):
        builder = build.Builder(mock.Mock())
        selected = mock.Mock(revision="abc")
        with mock.patch.object(builder, "_load", return_value=selected, create=True), \
                mock.patch("brood.dev.build.context.export") as export:
            with tempfile.TemporaryDirectory() as tmp:
                builder.context(Path(tmp) / "context")
        export.assert_called_once()
        self.assertIs(export.call_args.args[0], selected)

    def git(self, root, *args):
        result = subprocess.run(["git", "-C", str(root), *args], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        return result.stdout.strip()

    def test_snapshot_excludes_ignored_settings_and_preserves_the_commit(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            source, target = root / "source", root / "snapshot"
            source.mkdir()
            self.git(source, "init", "-q")
            self.git(source, "config", "user.name", "Test")
            self.git(source, "config", "user.email", "test@example.invalid")
            (source / ".gitignore").write_text(".env\nsecrets/\n")
            (source / "code.py").write_text("value = 1\n")
            (source / ".env").write_text("GITLAB_TOKEN=private")
            (source / "secrets").mkdir()
            (source / "secrets/key").write_text("private")
            self.git(source, "add", ".")
            self.git(source, "commit", "-qm", "Test snapshot")
            self.git(source, "remote", "add", "origin", "https://example.org/project.git")
            sha = self.git(source, "rev-parse", "HEAD")
            self.assertEqual(build.snapshot(source, "HEAD", target), sha)
            self.assertEqual(self.git(target, "rev-parse", "HEAD"), sha)
            self.assertEqual(self.git(target, "remote", "get-url", "origin"),
                             "https://example.org/project.git")
            self.assertEqual((target / "code.py").read_text(), "value = 1\n")
            self.assertFalse((target / ".env").exists())
            self.assertFalse((target / "secrets").exists())
            self.assertNotIn(str(source), (target / ".git/config").read_text())
            self.assertFalse((target / ".git/FETCH_HEAD").exists())
            self.git(source, "remote", "set-url", "origin", "ssh://git:private@example.org/project.git")
            with self.assertRaisesRegex(RuntimeError, "credentials"):
                build.snapshot(source, "HEAD", root / "unsafe-snapshot")

    def test_context_can_be_exported_without_publishing_an_image(self):
        from brood.dev.workbench.cli import build as command
        with tempfile.TemporaryDirectory() as tmp, mock.patch.object(command, "cluster"), \
                mock.patch.object(command, "Builder") as builder, mock.patch("builtins.print"):
            path = str(Path(tmp) / "context")
            self.assertEqual(command.main(["--export", path]), 0)
            builder.return_value.context.assert_called_once_with(path, "HEAD")
            builder.return_value.build.assert_not_called()

    def test_image_pins_the_verified_native_cli_and_terminal_tools(self):
        text = (build.HERE / "Dockerfile").read_text()
        self.assertIn("ARG CLAUDE_VERSION=2.1.310", text)
        self.assertIn("ARG CLAUDE_SHA256=b73676430af12f97316a798b8371f81d640f90a58bdcb8f3ce56a835bb47df56", text)
        self.assertIn("https://downloads.claude.ai/claude-code-releases/${CLAUDE_VERSION}/linux-x64/claude", text)
        self.assertIn('"${CLAUDE_SHA256}"', text)
        self.assertIn("ttyd", text)
        self.assertIn("tmux", text)
        self.assertIn("claude --version", text)

    def test_terminal_is_built_from_pinned_source_not_an_unavailable_package(self):
        text = (build.HERE / "Dockerfile").read_text()
        self.assertIn("ARG TTYD_VERSION=1.7.7", text)
        self.assertIn("ARG TTYD_SHA256=039dd995229377caee919898b7bd54484accec3bba49c118e2d5cd6ec51e3650", text)
        self.assertIn("https://codeload.github.com/tsl0922/ttyd/tar.gz/refs/tags/${TTYD_VERSION}", text)
        self.assertIn('"${TTYD_SHA256}"', text)
        self.assertIn("cmake --build", text)
        self.assertIn("libwebsockets-evlib-uv", text)
        self.assertNotIn("e2fsprogs tmux ttyd", text)

    def test_image_declares_tools_for_its_private_ext4_storage(self):
        self.assertIn("e2fsprogs", (build.HERE / "Dockerfile").read_text())

    def test_context_inside_loader_is_refused_before_copying(self):
        with tempfile.TemporaryDirectory() as tmp:
            here = Path(tmp) / "loader"
            here.mkdir()
            target = here / "context"
            builder = build.Builder(mock.Mock())
            with mock.patch.object(build, "HERE", here), mock.patch.object(
                    build, "snapshot", side_effect=AssertionError("copy started before validation")):
                with self.assertRaisesRegex(RuntimeError, "outside.*workbench"):
                    builder.context(target)
            self.assertFalse(target.exists())

    def test_commands_are_discovered(self):
        self.assertEqual(set(cli.groups().get("workbench", {})), {"build", "template", "start", "run"})
