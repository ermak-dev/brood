"""Образ получает только Git-снимки и закреплённые инструменты проекта."""
import json
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from brood.common import cli
from brood.dev.workbench import build


class ImageBuildTests(unittest.TestCase):
    # FEATURE: Сборка workbench делегирует Go, не Python compiler/schema.
    # SPEC: Committed Git export сохраняет identity, исключает secrets и credentials.
    # IMPLEMENTATION: Реальный Go export проверяется на локальном Git repository.
    # RESULT: IMPLEMENTED — identity и secret isolation сохранены без Python compiler.
    # STATUS: DONE
    @classmethod
    def setUpClass(cls):
        cls.tools = tempfile.TemporaryDirectory()
        cls.client = str(Path(cls.tools.name) / "bro")
        result = subprocess.run(["go", "build", "-o", cls.client, "./cmd/bro"],
                                cwd=build.HERE.parents[2] / "go", capture_output=True, text=True)
        if result.returncode:
            raise RuntimeError(result.stderr)

    @classmethod
    def tearDownClass(cls):
        cls.tools.cleanup()

    def test_context_delegates_to_current_directory_without_git_selection(self):
        with tempfile.TemporaryDirectory() as tmp:
            builder = build.Builder(project=tmp)
            with mock.patch.object(build.subprocess, "run", return_value=mock.Mock(
                    returncode=0, stdout="context\n", stderr="")) as run:
                builder.context(Path(tmp) / "context")
            args = run.call_args.args[0]
            self.assertEqual(args[:3], ["bro", "build", "development"])
            self.assertIn("--export", args)
            self.assertNotIn("--ref", args)
            self.assertNotIn("--registry", args)
            self.assertNotIn(tmp, args)
            self.assertEqual(run.call_args.kwargs["cwd"], Path(tmp))
            self.assertTrue(run.call_args.kwargs["capture_output"])

    def test_build_keeps_go_published_artifacts_and_returns_only_the_image(self):
        builder = build.Builder()
        with mock.patch.object(build.subprocess, "run", return_value=mock.Mock(
                returncode=0, stdout="registry.example/image@sha256:abc\n/project/.brood/build/development\n",
                stderr="")) as run:
            self.assertEqual(builder.build(), "registry.example/image@sha256:abc")
        self.assertNotIn("--export", run.call_args.args[0])

    def test_empty_go_build_output_has_a_reason_without_a_traceback(self):
        with mock.patch.object(build.subprocess, "run", return_value=mock.Mock(
                returncode=0, stdout="", stderr="")):
            with self.assertRaisesRegex(RuntimeError, "Go.*no image"):
                build.Builder().build()

    def test_missing_client_is_an_actionable_refusal(self):
        with mock.patch.object(build.subprocess, "run", side_effect=FileNotFoundError):
            with self.assertRaisesRegex(RuntimeError, "bro.*not installed"):
                build.Builder().build()

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
            declaration = (build.HERE.parents[2] / "go/brood/build/testdata/minimal.yaml").read_text()
            declaration = declaration.replace("destination: project", "destination: project\n    gitMetadata: true")
            (source / "Broodfile").write_text(declaration)
            (source / "environment").mkdir()
            (source / "environment/Dockerfile").write_text("FROM scratch\n")
            (source / ".env").write_text("GITLAB_TOKEN=private")
            (source / "secrets").mkdir()
            (source / "secrets/key").write_text("private")
            self.git(source, "add", ".")
            self.git(source, "commit", "-qm", "Test snapshot")
            self.git(source, "remote", "add", "origin", "https://example.org/project.git")
            sha = self.git(source, "rev-parse", "HEAD")
            builder = build.Builder(project=source, client=self.client)
            with mock.patch.object(build.config, "get", side_effect=lambda key:
                                   {"BUILD_REGISTRY": "registry.example/team",
                                    "BUILD_ATESPACE": "dev"}.get(key)):
                builder.context(target)
            target = target / "project"
            self.assertEqual(self.git(target, "rev-parse", "HEAD"), sha)
            self.assertEqual(self.git(target, "remote", "get-url", "origin"),
                             "https://example.org/project.git")
            self.assertEqual((target / "code.py").read_text(), "value = 1\n")
            self.assertFalse((target / ".env").exists())
            self.assertFalse((target / "secrets").exists())
            self.assertNotIn(str(source), (target / ".git/config").read_text())
            self.assertFalse((target / ".git/FETCH_HEAD").exists())
            self.git(source, "remote", "set-url", "origin", "ssh://git:private@example.org/project.git")
            with self.assertRaisesRegex(RuntimeError, "credentials|repository"), \
                    mock.patch.object(build.config, "get", side_effect=lambda key:
                                      {"BUILD_REGISTRY": "registry.example/team",
                                       "BUILD_ATESPACE": "dev"}.get(key)):
                builder.context(root / "unsafe-snapshot")
            self.assertFalse((root / "unsafe-snapshot").exists())

    def test_context_can_be_exported_without_publishing_an_image(self):
        from brood.dev.workbench.cli import build as command
        with tempfile.TemporaryDirectory() as tmp, mock.patch.object(command, "cluster",
                side_effect=AssertionError("export must not inspect a host cluster"), create=True), \
                mock.patch.object(command, "Builder") as builder, mock.patch("builtins.print"):
            path = str(Path(tmp) / "context")
            self.assertEqual(command.main(["--export", path]), 0)
            builder.return_value.context.assert_called_once_with(path)
            builder.return_value.build.assert_not_called()

    def test_image_pins_the_verified_native_cli_and_terminal_tools(self):
        text = (build.HERE / "Dockerfile").read_text()
        self.assertIn("ARG CLAUDE_VERSION=2.1.310", text)
        self.assertIn("ARG CLAUDE_SHA256=b73676430af12f97316a798b8371f81d640f90a58bdcb8f3ce56a835bb47df56", text)
        # Пин claude общий для всей установки (#155/#156): workbench и pins.json
        # обязаны совпадать, иначе версии разъедутся молча.
        pins = json.loads((build.HERE.parent / "pins.json").read_text())
        self.assertEqual((pins["CLAUDE_VERSION"], pins["CLAUDE_SHA256"]),
                         ("2.1.310", "b73676430af12f97316a798b8371f81d640f90a58bdcb8f3ce56a835bb47df56"))
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
            builder = build.Builder()
            with mock.patch.object(build, "HERE", here), mock.patch.object(
                    build.subprocess, "run", side_effect=AssertionError("copy started before validation")):
                with self.assertRaisesRegex(RuntimeError, "outside.*workbench"):
                    builder.context(target)
            self.assertFalse(target.exists())

    def test_commands_are_discovered(self):
        self.assertEqual(set(cli.groups().get("workbench", {})), {"build", "template", "start", "run", "prepare"})
