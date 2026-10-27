"""Dev test обнаруживает пакеты и не принимает пустой прогон за успех."""
from pathlib import Path
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[3]


class TestCommandTests(unittest.TestCase):
    def test_package_label_discovers_tests_from_another_directory(self):
        with tempfile.TemporaryDirectory() as cwd:
            result = subprocess.run([str(ROOT / "bin/brood"), "dev", "test", "brood.common.tests"],
                                    cwd=cwd, text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertRegex(result.stderr, r"Ran [1-9][0-9]* tests")

    def test_single_test_label(self):
        result = subprocess.run([str(ROOT / "bin/brood"), "dev", "test",
                                 "brood.dev.tests.test_setup.SetupTests.test_append_lines"],
                                cwd=ROOT, text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("Ran 1 test", result.stderr)

    def test_unknown_label_is_a_failure(self):
        result = subprocess.run([str(ROOT / "bin/brood"), "dev", "test", "brood.no_such_tests"],
                                cwd=ROOT, text=True, capture_output=True)
        self.assertNotEqual(result.returncode, 0)
