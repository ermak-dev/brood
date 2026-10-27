"""Broodfile и контекст читаются из одного commit, не из рабочей копии."""
import json
from pathlib import Path

from brood.dev.build import context, project
from brood.dev.build.tests.test_sources import GitCase


class ProjectTests(GitCase):
    def setUp(self):
        super().setUp()
        fixture = Path(__file__).with_name("fixtures") / "minimal.yaml"
        (self.repo / "Broodfile").write_text(fixture.read_text())
        (self.repo / "environment").mkdir()
        (self.repo / "environment/Dockerfile").write_text("FROM scratch\n")
        self.commit()

    def load(self, **kwargs):
        return project.load(self.repo, registry="localhost:5002", atespace="build-test", **kwargs)

    def test_reader_uses_selected_commit_and_reports_excluded_changes(self):
        (self.repo / "Broodfile").write_text("invalid working tree")
        loaded = self.load()
        self.assertEqual(loaded.plan["project"], "example")
        self.assertEqual(loaded.revision, self.sha)
        self.assertTrue(loaded.dirty)
        self.assertEqual(loaded.root, self.repo)

    def test_export_is_portable_and_contains_no_working_tree_overlays(self):
        loaded = self.load()
        (self.repo / "code.py").write_text("dirty")
        exported = self.root / "export"
        reports = context.export(loaded, exported)
        self.assertEqual((exported / "project/code.py").read_text(), "version = 1\n")
        self.assertEqual(reports["project"]["revision"], self.sha)
        plan = json.loads((exported / "build-plan.json").read_text())
        self.assertEqual(plan["sources"]["project"]["revision"], self.sha)
        self.assertNotIn(str(self.repo), json.dumps(plan))

    def test_runtime_only_target_fields_do_not_change_context_metadata(self):
        from brood.dev.build.tests.test_plan import minimal
        document = minimal()
        document["templates"]["coding"] = json.loads(json.dumps(document["templates"]["python"]))
        document["templates"]["coding"]["actor"]["containers"][0]["args"] = ["interactive"]
        document["targets"]["coding"] = {"template": "coding"}
        (self.repo / "Broodfile").write_text(json.dumps(document))
        self.commit()
        first, second = self.root / "first", self.root / "second"
        context.export(self.load(), first)
        context.export(self.load(target="coding"), second)
        self.assertEqual((first / "build-plan.json").read_bytes(), (second / "build-plan.json").read_bytes())

    def test_export_does_not_overwrite_an_existing_path(self):
        exported = self.root / "export"
        exported.mkdir()
        (exported / "mine").write_text("keep")
        with self.assertRaisesRegex(RuntimeError, "exists"):
            context.export(self.load(), exported)
        self.assertEqual((exported / "mine").read_text(), "keep")

    def test_missing_dockerfile_leaves_no_partial_context(self):
        (self.repo / "environment/Dockerfile").unlink()
        self.commit()
        exported = self.root / "export"
        with self.assertRaisesRegex(RuntimeError, "Dockerfile"):
            context.export(self.load(), exported)
        self.assertFalse(exported.exists())

    def test_project_without_origin_can_render_artifacts_when_metadata_is_disabled(self):
        from brood.dev.build import artifacts
        self.git("remote", "remove", "origin")
        selected = self.load()
        reports = context.export(selected, self.root / "export")
        result = artifacts.render(selected.plan, "localhost:5002/example/python@sha256:" + "a" * 64, reports)
        self.assertNotIn("repository", result["build.json"]["sources"]["project"])

    def test_pins_read_from_the_same_commit(self):
        text = (self.repo / "Broodfile").read_text().replace(
            "name: example", "name: example\npinsFile: pins.json").replace(
            "platform: linux/amd64", "platform: linux/amd64\n      argsFromPins: [VERSION]")
        (self.repo / "Broodfile").write_text(text)
        (self.repo / "pins.json").write_text('{"VERSION": "1"}')
        self.commit()
        (self.repo / "pins.json").write_text('{"VERSION": "dirty"}')
        self.assertEqual(self.load().plan["build"]["arguments"], {"VERSION": "1"})
