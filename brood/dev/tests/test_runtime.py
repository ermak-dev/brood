"""Один Go-runtime; Python остаётся инструментами разработки."""
import subprocess
from pathlib import Path
import tomllib
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[3]


class RuntimeBoundaryTests(unittest.TestCase):
    def test_legacy_runtime_sources_are_removed(self):
        for name in ("brood/server/manage.py", "brood/server/web/package.json",
                     "brood/client/__init__.py", "brood/dev/build/plan.py"):
            with self.subTest(path=name):
                self.assertFalse((ROOT / name).exists(), f"legacy source remains: {name}")

    def test_python_dependencies_do_not_install_server_runtime(self):
        project = tomllib.loads((ROOT / "pyproject.toml").read_text())["project"]
        dependencies = project.get("dependencies", []) + [
            value for group in project.get("optional-dependencies", {}).values() for value in group]
        for dependency in dependencies:
            self.assertNotRegex(dependency.lower(), r"^(django|gunicorn|psycopg)(\W|$)")

    def test_public_rollout_is_gated_by_successful_default_branch_checks(self):
        pipeline = yaml.safe_load((ROOT / ".gitlab-ci.yml").read_text())
        deploy = pipeline["deploy"]
        self.assertEqual(deploy["stage"], "deploy")
        self.assertLess(pipeline["stages"].index("test"), pipeline["stages"].index("deploy"))
        self.assertEqual(deploy["rules"], [{"if": "$CI_COMMIT_BRANCH == $CI_DEFAULT_BRANCH"}])
        self.assertFalse(deploy.get("allow_failure", False))

    def test_dev_test_is_the_python_test_entrypoint(self):
        result = subprocess.run([str(ROOT / "bin/brood"), "dev", "test", "--help"],
                                cwd=ROOT, text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
        self.assertIn("unittest", result.stdout)

    def test_launcher_no_longer_advertises_legacy_commands(self):
        result = subprocess.run([str(ROOT / "bin/brood")], cwd=ROOT, text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("Django", result.stdout)
        self.assertNotRegex(result.stdout, r"(?m)^  (server|build)\s")
