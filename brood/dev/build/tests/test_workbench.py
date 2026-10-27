"""Две цели Brood используют одно окружение и явно объявляют права гостя."""
import json
from pathlib import Path
import unittest

from brood.common import config
from brood.dev.build.plan import make_plan, parse_document
from brood.dev.substrate import local


class WorkbenchDeclarationTests(unittest.TestCase):
    def test_both_targets_share_the_image_inputs_not_the_runtime_command(self):
        root = Path(config.PROJECT)
        document = parse_document((root / "Broodfile").read_text())
        pins = json.loads((root / document["pinsFile"]).read_text())
        plans = [make_plan(document, pins, target, "localhost:5002", "build-test")
                 for target in ("development", "coding")]
        self.assertEqual(plans[0]["build"], plans[1]["build"])
        self.assertEqual(plans[0]["sources"], plans[1]["sources"])
        self.assertNotEqual(plans[0]["actor"]["containers"][0]["args"],
                            plans[1]["actor"]["containers"][0]["args"])
        self.assertEqual(plans[0]["sources"]["substrate"]["revision"], local.COMMIT)
        for plan in plans:
            self.assertEqual(plan["actor"]["resources"]["limits"],
                             {"cpuMillicores": "4000", "memoryBytes": "8589934592"})
            self.assertEqual(len(plan["actor"]["containers"][0]["securityContext"]["capabilities"]["add"]), 41)
            self.assertNotIn("*", json.dumps(plan["network"]))
        self.assertIn("api.anthropic.com", plans[1]["network"]["tls"]["hosts"])
        self.assertNotIn("api.anthropic.com", plans[0]["network"]["tls"]["hosts"])

    def test_dockerfile_contains_no_runtime_overlays(self):
        text = (Path(config.PROJECT) / "brood/dev/workbench/Dockerfile").read_text()
        self.assertNotIn("COPY runtime/", text)
        self.assertNotIn("COPY workbench.sh", text)

    def test_pins_are_shared_without_parsing_shell_assignments(self):
        root = Path(config.PROJECT)
        pins = json.loads((root / "brood/dev/pins.json").read_text())
        self.assertEqual(pins["SUBSTRATE_COMMIT"], local.COMMIT)
        self.assertIn("@sha256:", pins["PYTHON_IMAGE"])
        self.assertIn("brood/dev/pins.json", (root / ".ci/test.sh").read_text())
        self.assertIn("brood/dev/pins.json", (root / "deploy/provision.sh").read_text())
