"""Проверки независимых манифестов и атомарной публикации комплекта."""

import copy
from concurrent.futures import ThreadPoolExecutor
import hashlib
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from brood.dev.build import artifacts
from brood.dev.build.tests.test_plan import planned


def image(plan, digit="a"):
    return f"{plan['registry']}/{plan['project']}/{plan['environment']}@sha256:{digit * 64}"


class RenderTests(unittest.TestCase):
    def test_render_is_independent_and_sets_protojson_defaults(self):
        plan = planned()
        before = copy.deepcopy(plan)
        first = artifacts.render(plan, image(plan))
        second = artifacts.render(plan, image(plan))
        self.assertEqual(set(first), {"actor-template.json", "egress-policy.json", "build.json"})
        self.assertEqual(first, second)
        self.assertEqual(json.loads(json.dumps(first)), first)
        actor = first["actor-template.json"]
        self.assertEqual(actor["containers"][0]["image"], image(plan))
        self.assertEqual(actor["metadata"]["atespace"], plan["atespace"])
        self.assertRegex(actor["metadata"]["name"], r"^[a-z0-9][a-z0-9-]{0,61}[a-z0-9]$")
        self.assertEqual(actor["snapshotConfig"]["goldenSnapshotConfig"],
                         {"mode": "GOLDEN_SNAPSHOT_MODE_DISABLED"})
        self.assertEqual(first["egress-policy.json"], {
            "metadata": {"atespace": plan["atespace"], "name": "default"}, "rules": []})
        actor["containers"][0]["command"].append("changed")
        first["build.json"]["sources"]["project"]["destination"] = "changed"
        self.assertEqual(plan, before)
        self.assertEqual(second, artifacts.render(plan, image(plan)))

    def test_exact_egress_rules_and_snapshot_settings_preserved(self):
        plan = planned()
        plan["network"] = {"tls": {"hosts": ["github.com"], "ports": [443, 8443]},
                           "http": {"hosts": ["deb.debian.org"], "ports": [80]}}
        plan["actor"]["snapshotConfig"] = {"preferredFidelity": "SNAPSHOT_FIDELITY_MEMORY"}
        result = artifacts.render(plan, image(plan))
        self.assertEqual(result["egress-policy.json"]["rules"], [
            {"tlsPassthrough": {"hostnames": ["github.com"], "ports": {"numbers": [443, 8443]}}},
            {"http": {"hostnames": ["deb.debian.org"], "ports": {"numbers": [80]}}},
        ])
        self.assertEqual(result["actor-template.json"]["snapshotConfig"]["preferredFidelity"],
                         "SNAPSHOT_FIDELITY_MEMORY")

    def test_template_hash_changes_for_runtime_network_image_and_identity(self):
        plan = planned()
        original = artifacts.render(plan, image(plan))
        name = original["actor-template.json"]["metadata"]["name"]
        variants = []
        for key, value in [("target", "coding"), ("atespace", "other"),
                           ("network", {"tls": {"hosts": ["github.com"], "ports": [443]}})]:
            changed = copy.deepcopy(plan)
            changed[key] = value
            variants.append(artifacts.render(changed, image(changed)))
        changed = copy.deepcopy(plan)
        changed["actor"]["containers"][0]["command"] = ["bash"]
        variants.extend([artifacts.render(changed, image(changed)), artifacts.render(plan, image(plan, "b"))])
        for result in variants:
            self.assertNotEqual(result["actor-template.json"]["metadata"]["name"], name)
            self.assertNotEqual(result["build.json"]["templateDigest"], original["build.json"]["templateDigest"])
        actor = copy.deepcopy(original["actor-template.json"])
        del actor["metadata"]
        canonical = json.dumps({"actor": actor, "network": plan["network"],
                                "identity": {key: plan[key] for key in
                                             ("project", "target", "environment", "atespace")}},
                               sort_keys=True, separators=(",", ":"), ensure_ascii=True)
        self.assertEqual(original["build.json"]["templateDigest"],
                         "sha256:" + hashlib.sha256(canonical.encode()).hexdigest())
        reordered = dict(reversed(list(plan.items())))
        self.assertEqual(artifacts.render(reordered, image(plan)), original)
        plan["target"] = "a" * 63
        self.assertLessEqual(len(artifacts.render(plan, image(plan))["actor-template.json"]["metadata"]["name"]), 63)

    def test_provenance_whitelists_reports_and_includes_build_inputs(self):
        plan = planned()
        plan["build"]["arguments"] = {"VERSION": "3.12"}
        plan["pins"] = {"VERSION": "3.12"}
        reports = {"project": {"revision": "c" * 40, "repository": "https://github.com/team/repo",
                               "path": "/machine/cache", "token": "private-value"}}
        result = artifacts.render(plan, image(plan), reports)["build.json"]
        self.assertEqual(result["apiVersion"], plan["apiVersion"])
        self.assertEqual(result["build"], plan["build"])
        self.assertEqual(result["pins"], plan["pins"])
        self.assertEqual(result["image"], image(plan))
        self.assertEqual(result["imageDigest"], "sha256:" + "a" * 64)
        self.assertEqual(result["sources"]["project"]["revision"], "c" * 40)
        self.assertEqual(result["sources"]["project"]["repository"], "https://github.com/team/repo")
        self.assertNotIn("/machine", json.dumps(result))
        self.assertNotIn("private-value", json.dumps(result))
        reports["project"]["revision"] = "d" * 40
        self.assertEqual(result["sources"]["project"]["revision"], "c" * 40)
        for report in [{"revision": "bad"}, {"repository": "https://user:private-value@github.com/repo"}]:
            with self.subTest(report=report), self.assertRaises(RuntimeError) as caught:
                artifacts.render(plan, image(plan), {"project": report})
            self.assertNotIn("private-value", str(caught.exception))

    def test_digest_and_uint_refusals(self):
        plan = planned()
        for value in [None, "", image(plan).replace("sha256:", "sha512:"), image(plan)[:-1],
                      image(plan).replace("/example/", "/other/"), image(plan).replace("a" * 64, "A" * 64),
                      image(plan) + ":tag"]:
            with self.subTest(value=value), self.assertRaises(RuntimeError):
                artifacts.render(plan, value)
        for value in ["0", "-1", str(2**64), "oops", True]:
            changed = copy.deepcopy(plan)
            changed["actor"]["resources"]["limits"]["memoryBytes"] = value
            with self.subTest(value=value), self.assertRaises(RuntimeError):
                artifacts.render(changed, image(plan))


class PublishTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.plan = planned()
        self.bundle = artifacts.render(self.plan, image(self.plan))
        self.output = self.root / ".brood/build/development"

    def publish(self, bundle=None):
        return artifacts.publish(self.root, "development", self.bundle if bundle is None else bundle)

    def test_complete_immutable_runs_and_atomic_stable_pointer(self):
        stable = self.publish()
        self.assertEqual(stable, self.output)
        self.assertTrue(stable.is_symlink())
        first = stable.resolve()
        self.assertEqual(first.parent, self.root / ".brood/build/.runs/development")
        self.assertEqual(set(p.name for p in first.iterdir()), set(self.bundle))
        for name, value in self.bundle.items():
            self.assertEqual(json.loads((stable / name).read_text()), value)
        updated = artifacts.render(self.plan, image(self.plan, "b"))
        replace = os.replace

        def checked_replace(source, destination):
            self.assertEqual(json.loads((stable / "build.json").read_text()), self.bundle["build.json"])
            self.assertTrue(Path(source).is_symlink())
            self.assertEqual(set(p.name for p in Path(source).resolve().iterdir()), set(updated))
            replace(source, destination)

        with patch.object(artifacts.os, "replace", side_effect=checked_replace):
            self.assertEqual(self.publish(updated), stable)
        self.assertNotEqual(stable.resolve(), first)
        self.assertEqual(json.loads((first / "build.json").read_text()), self.bundle["build.json"])

    def test_write_and_replace_failures_preserve_previous_success(self):
        self.publish()
        previous = self.output.resolve()
        for operation in [patch.object(Path, "write_text", side_effect=OSError("disk full")),
                          patch.object(artifacts.os, "replace", side_effect=OSError("replace failed"))]:
            with self.subTest(operation=operation), operation, self.assertRaises(RuntimeError):
                self.publish()
            self.assertEqual(self.output.resolve(), previous)
            self.assertEqual(json.loads((self.output / "build.json").read_text()), self.bundle["build.json"])

    def test_foreign_files_directories_links_and_tampered_marker_are_refused(self):
        self.output.parent.mkdir(parents=True)
        for kind in ["file", "directory", "link"]:
            if kind == "file":
                self.output.write_text("mine")
            elif kind == "directory":
                self.output.mkdir()
            else:
                self.output.symlink_to(self.root / "missing")
            with self.subTest(kind=kind), self.assertRaisesRegex(RuntimeError, "foreign|owned"):
                self.publish()
            if kind == "directory":
                self.output.rmdir()
            else:
                self.output.unlink()
        self.publish()
        marker = self.output / "build.json"
        marker.write_text("{}")
        previous = os.readlink(self.output)
        with self.assertRaisesRegex(RuntimeError, "foreign|owned"):
            self.publish()
        self.assertEqual(os.readlink(self.output), previous)
        self.assertEqual(marker.read_text(), "{}")

    def test_symlink_parents_and_target_traversal_are_refused(self):
        for target in ["", ".", "..", "../escape", "a/b", "/tmp/escape", "UPPER", "a" * 64]:
            with self.subTest(target=target), self.assertRaises(RuntimeError):
                artifacts.publish(self.root, target, self.bundle)
        for relative in [".brood", ".brood/build", ".brood/build/.runs", ".brood/build/.runs/development"]:
            with self.subTest(parent=relative), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                link = root / relative
                link.parent.mkdir(parents=True, exist_ok=True)
                elsewhere = root / "elsewhere"
                elsewhere.mkdir()
                link.symlink_to(elsewhere, target_is_directory=True)
                with self.assertRaisesRegex(RuntimeError, "symlink|directory"):
                    artifacts.publish(root, "development", self.bundle)
                self.assertEqual(list(elsewhere.iterdir()), [])
        alias = self.root / "alias"
        alias.symlink_to(self.root, target_is_directory=True)
        with self.assertRaisesRegex(RuntimeError, "symlink|directory"):
            artifacts.publish(alias, "development", self.bundle)

    def test_same_target_publications_are_serialized(self):
        with ThreadPoolExecutor(max_workers=4) as pool:
            results = list(pool.map(lambda _: self.publish(), range(8)))
        self.assertEqual(results, [self.output] * 8)
        runs = self.root / ".brood/build/.runs/development"
        self.assertEqual(len(list(runs.iterdir())), 8)
        for run in runs.iterdir():
            self.assertEqual(set(p.name for p in run.iterdir()), set(self.bundle))
        self.assertEqual(json.loads((self.output / "build.json").read_text()), self.bundle["build.json"])

    def test_name_collisions_never_delete_unrelated_paths(self):
        self.publish()
        runs = self.root / ".brood/build/.runs/development"
        identifier = "1" * 32
        collision = runs / ("run-" + identifier)
        collision.mkdir()
        sentinel = collision / "mine"
        sentinel.write_text("keep")
        with patch.object(artifacts.uuid, "uuid4") as random:
            random.return_value.hex = identifier
            with self.assertRaises(RuntimeError):
                self.publish()
        self.assertEqual(sentinel.read_text(), "keep")
        pointer = self.output.parent / (".pointer-" + identifier)
        pointer.write_text("keep")
        with patch.object(artifacts.uuid, "uuid4") as random:
            random.return_value.hex = identifier
            collision.rename(runs / "saved")
            with self.assertRaises(RuntimeError):
                self.publish()
        self.assertEqual(pointer.read_text(), "keep")

    def test_invalid_bundle_never_replaces_success(self):
        self.publish()
        previous = self.output.resolve()
        for bundle in [{}, {**self.bundle, "../escape": {}},
                       {**self.bundle, "build.json": {}},
                       {**self.bundle, "actor-template.json": {"bad": object()}}]:
            with self.subTest(keys=list(bundle)), self.assertRaises(RuntimeError):
                self.publish(bundle)
            self.assertEqual(self.output.resolve(), previous)
