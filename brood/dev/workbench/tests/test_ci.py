"""Контракты живых CI и приёмки внутри гостя."""
from pathlib import Path
import unittest


ROOT = Path(__file__).resolve().parents[4]


class GoAcceptanceTests(unittest.TestCase):
    # IMPLEMENTATION: Общий pinned toolchain и приватный Go endpoint вместо public cutover.
    # RESULT: IMPLEMENTED — контракты CI, pins и release artifacts проверены.
    # STATUS: DONE
    def test_ci_uses_stdlib_developer_tests(self):
        script = (ROOT / ".ci/test.sh").read_text()
        self.assertIn("bin/brood dev test -v", script)
        self.assertNotIn("server test", script)
        self.assertNotIn("DJANGO", script)

    def test_guest_runs_real_go_checks_before_isolated_deployment(self):
        script = (ROOT / ".ci/workbench.sh").read_text()
        for command in ("bin/brood dev test -v", "bin/brood dev web test", "go test ./...", "go build",
                        "bin/brood substrate start", "bin/brood deploy go"):
            self.assertIn(command, script)
        self.assertLess(script.index("go test ./..."), script.index("bin/brood deploy go"))
        self.assertNotIn("bin/brood deploy server", script)
        self.assertLess(script.index("bin/brood workbench prepare"), script.index("bin/brood deploy go"))
        for obsolete in ("server test", "npm", "django", "brood build"):
            self.assertNotIn(obsolete, script)

    def test_guest_image_installs_only_dev_python_dependencies(self):
        dockerfile = (ROOT / "brood/dev/workbench/Dockerfile").read_text()
        self.assertNotIn('["server"]', dockerfile)
        self.assertIn('["dev"]', dockerfile)
        self.assertIn('"${GO_SHA256}"', dockerfile)
        self.assertIn("GOTOOLCHAIN=local", dockerfile)

    def test_manifests_do_not_import_the_removed_python_compiler(self):
        source = (ROOT / "brood/dev/workbench/manifest.py").read_text()
        self.assertNotIn("brood.dev.build", source)
        runtime = (ROOT / "brood/dev/workbench/runtime.py").read_text()
        self.assertNotIn("brood.dev.deploy.server", runtime)

    def test_python_ci_installs_the_pinned_go_client_test_toolchain(self):
        script = (ROOT / ".ci/test.sh").read_text()
        self.assertIn(".ci/go-toolchain.sh", script)
        helper = ROOT / ".ci/go-toolchain.sh"
        self.assertTrue(helper.is_file(), "shared pinned Go installer is missing")
        text = helper.read_text()
        self.assertIn("GO_SHA256", text)
        self.assertIn("sha256sum", text)
        self.assertIn("GOTOOLCHAIN=local", text)
        self.assertIn(".ci/go-toolchain.sh", (ROOT / ".ci/go-test.sh").read_text())

    def test_release_artifacts_remain_available(self):
        script = (ROOT / ".ci/go-test.sh").read_text()
        pipeline = (ROOT / ".gitlab-ci.yml").read_text()
        for command in ("make test", "make build", "make cross"):
            self.assertIn(command, script)
        self.assertIn("go/bin/*.tar.gz", pipeline)
