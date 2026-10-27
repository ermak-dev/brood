"""Go-сервер сохраняет исходную React-панель и её сборочный контракт."""
import json
import subprocess
from pathlib import Path
import unittest


ROOT = Path(__file__).resolve().parents[3]


class FrontendTests(unittest.TestCase):
    def test_web_group_explains_available_commands(self):
        result = subprocess.run([str(ROOT / "bin/brood"), "dev", "web"],
                                text=True, capture_output=True, cwd=ROOT)
        self.assertNotIn("Traceback", result.stderr)
        self.assertIn("brood dev web build", result.stdout + result.stderr)
        self.assertIn("brood dev web test", result.stdout + result.stderr)

    def test_original_react_frontend_is_retained(self):
        package = ROOT / "web/package.json"
        self.assertTrue(package.is_file(), "the existing React frontend must be restored")
        project = json.loads(package.read_text())
        self.assertIn("react", project["dependencies"])
        self.assertEqual(project["packageManager"], "bun@1.3.14")
        for path in ("src/features/providers", "src/features/authFiles", "src/features/substrate",
                     "src/router/MainRoutes.tsx", "AGENTS.md", "bun.lock"):
            with self.subTest(path=path):
                self.assertTrue((package.parent / path).exists())
