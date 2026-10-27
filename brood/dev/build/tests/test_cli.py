"""План и экспорт доступны без Docker, публикации и доступа к кластеру."""
from contextlib import redirect_stderr, redirect_stdout
import io
import json
from unittest.mock import patch

from brood.dev.build import cli
from brood.dev.build.tests import test_project


class CliTests(test_project.ProjectTests):
    def invoke(self, *args):
        stdout, stderr = io.StringIO(), io.StringIO()
        with redirect_stdout(stdout), redirect_stderr(stderr):
            code = cli.main(["--project", str(self.repo), "--registry", "localhost:5002",
                             "--atespace", "build-test", *args])
        self.assertEqual(code, 0)
        return stdout.getvalue(), stderr.getvalue()

    def test_plan_does_not_construct_a_cluster(self):
        with patch("brood.dev.build.cli.cluster") as cluster:
            output, _ = self.invoke("--plan")
        cluster.assert_not_called()
        self.assertEqual(json.loads(output)["target"], "development")

    def test_export_does_not_construct_a_cluster(self):
        target = self.root / "export"
        with patch("brood.dev.build.cli.cluster") as cluster:
            output, _ = self.invoke("--export", str(target))
        cluster.assert_not_called()
        self.assertIn(str(target), output)
        self.assertTrue((target / "build-plan.json").is_file())

    def test_excluded_changes_are_reported_on_stderr_not_in_json(self):
        (self.repo / "code.py").write_text("dirty")
        output, warning = self.invoke("--plan")
        self.assertEqual(json.loads(output)["project"], "example")
        self.assertIn("excluded", warning)
