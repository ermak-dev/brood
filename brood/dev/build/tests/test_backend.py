"""Digest берётся из metadata конкретной сборки, а не общего тега."""
import json
from types import SimpleNamespace
from unittest.mock import Mock

from brood.dev.build import backend
from brood.dev.build.tests.test_project import ProjectTests


class BackendTests(ProjectTests):
    def test_build_uses_unique_tags_and_its_own_metadata(self):
        loaded = self.load()
        calls = []

        def run(argv, **kwargs):
            calls.append(argv)
            metadata = argv[argv.index("--metadata-file") + 1]
            from pathlib import Path
            Path(metadata).write_text(json.dumps({"containerimage.digest": "sha256:" + "a" * 64}))
            self.assertTrue(kwargs["capture_output"])
            return SimpleNamespace(returncode=0, stdout="build output", stderr="")

        cluster = SimpleNamespace(check=Mock(), shell=SimpleNamespace(env={}))
        first = backend.build(loaded, cluster, runner=run)
        backend.build(loaded, cluster, runner=run)
        self.assertEqual(first.image, "localhost:5002/example/python@sha256:" + "a" * 64)
        self.assertEqual(first.stdout, "build output")
        self.assertNotEqual(calls[0][calls[0].index("--tag") + 1], calls[1][calls[1].index("--tag") + 1])
        self.assertFalse(any("inspect" in call for call in calls))
        self.assertEqual(cluster.check.call_count, 2)

    def test_unowned_registry_is_refused_before_build(self):
        loaded = self.load()
        loaded.plan["registry"] = "foreign.example.org"
        cluster = SimpleNamespace(check=Mock())
        run = Mock()
        with self.assertRaisesRegex(RuntimeError, "owned.*registry"):
            backend.build(loaded, cluster, runner=run)
        cluster.check.assert_not_called()
        run.assert_not_called()

    def test_missing_metadata_is_not_replaced_with_tag_inspection(self):
        cluster = SimpleNamespace(check=Mock(), shell=SimpleNamespace(env={}))
        run = Mock(return_value=SimpleNamespace(returncode=0, stdout="", stderr=""))
        with self.assertRaisesRegex(RuntimeError, "metadata"):
            backend.build(self.load(), cluster, runner=run)
        self.assertEqual(run.call_count, 1)

    def test_ownership_refusal_precedes_builder(self):
        cluster = SimpleNamespace(check=Mock(side_effect=RuntimeError("foreign cluster")))
        run = Mock()
        with self.assertRaisesRegex(RuntimeError, "foreign cluster"):
            backend.build(self.load(), cluster, runner=run)
        run.assert_not_called()
