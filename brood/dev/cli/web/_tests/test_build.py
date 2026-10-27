"""Сборка исходной панели для standalone Go-сервера."""
from pathlib import Path
import unittest


ROOT = Path(__file__).resolve().parents[5]
BUN = "oven/bun:1.3.14-slim@sha256:d56a2534ffd262e92c12fd3249d3924d296d97086da773f821d7d0477435ea04"


# FEATURE: Release builds embed the original single-file React panel.
# SPEC: Pinned Bun, frozen lockfile, fixed panel version, no Bun runtime.
# IMPLEMENTATION: Shared dev/CI script and a Docker panel build stage.
# RESULT: IMPLEMENTED — build contract tests and the scratch image build pass.
# STATUS: DONE
class BuildTests(unittest.TestCase):
    def test_docker_rebuilds_frontend(self):
        dockerfile = (ROOT / "deploy/Dockerfile.go").read_text()
        self.assertIn(f"FROM {BUN} AS panel", dockerfile)
        self.assertIn("bun install --frozen-lockfile", dockerfile)
        self.assertIn("VERSION=v1.26.7+brood", dockerfile)
        self.assertIn("COPY --from=panel /app/dist/index.html ./brood/server/static/index.html", dockerfile)

    def test_release_checks_rebuild_source(self):
        self.assertIn("bash .ci/web.sh verify", (ROOT / ".ci/go-test.sh").read_text())

    def test_dev_commands_share_build_script(self):
        self.assertTrue((ROOT / "brood/dev/cli/web/_common.py").exists())
        from unittest.mock import patch
        from brood.dev.cli.web._common import run
        with patch("brood.dev.cli.web._common.subprocess.run") as call:
            call.return_value.returncode = 0
            self.assertEqual(run("build"), 0)
        argv = call.call_args.args[0]
        self.assertEqual(argv[-1], "build")
        self.assertTrue(argv[-2].endswith(".ci/web.sh"))
        with self.assertRaises(ValueError):
            run("build; arbitrary-command")
