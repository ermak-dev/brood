"""Проверки чистого разбора Broodfile и разрешения плана сборки."""

import copy
import json
from pathlib import Path
import traceback
import unittest

from brood.dev.build.plan import (
    make_plan,
    parse_document,
    public_hostname,
    repository,
    safe_path,
)


def minimal():
    return parse_document(Path(__file__).with_name("fixtures").joinpath("minimal.yaml").read_text())


def planned(document=None, **kwargs):
    context = {"registry": "localhost:5000/team", "atespace": "development"}
    context.update(kwargs)
    return make_plan(minimal() if document is None else document, **context)


def replace(document, path, value):
    for key in path[:-1]:
        document = document[key]
    document[path[-1]] = value


class ParseTests(unittest.TestCase):
    def test_minimal_independent_plain_json_document(self):
        first, second = minimal(), minimal()
        first["sources"]["project"]["destination"] = "changed"
        self.assertEqual(second["sources"]["project"]["destination"], "project")
        self.assertEqual(json.loads(json.dumps(second)), second)
        self.assertIs(type(second), dict)

    def test_yaml_features_rejected_without_disclosing_values(self):
        cases = [
            "name: secret-value\nname: duplicate\n",
            "name: &secret-value foo\n",
            "name: *secret-value\n",
            "name: {<<: {secret-value: test}}\n",
            "name: !!python/object:secret-value {}\n",
            "name: !secret-value foo\n",
            "1: secret-value\n",
            "? [secret-value]\n: test\n",
            "name: [secret-value\n",
            "---\nname: secret-value\n---\nname: second\n",
        ]
        for text in cases:
            with self.subTest(text=text), self.assertRaises(RuntimeError) as caught:
                parse_document(text, "custom.yaml")
            self.assertIn("custom.yaml", str(caught.exception))
            self.assertIn("line", str(caught.exception))
            self.assertNotIn("secret-value", str(caught.exception))

    def test_schema_errors_are_sanitized_and_located(self):
        text = Path(__file__).with_name("fixtures").joinpath("minimal.yaml").read_text()
        for invalid in [
            text.replace("name: example", "name: secret-value!"),
            text.replace("platform: linux/amd64", "platform: secret-value"),
            text + "secret-value: hidden\n",
            text.replace("command: [python3]", "command: [python3]\n          image: secret-value"),
            text.replace("type: project", "type: project\n    repository: secret-value"),
        ]:
            with self.subTest(invalid=invalid), self.assertRaises(RuntimeError) as caught:
                parse_document(invalid, "test.yaml")
            error = str(caught.exception)
            self.assertIn("test.yaml", error)
            self.assertIn("line", error)
            self.assertNotIn("secret-value", error)

    def test_empty_wrong_types_size_and_nesting_are_runtime_errors(self):
        for text in ["", "null", "[]", "name: true", "x" * (1024 * 1024 + 1),
                     "#" + "é" * (512 * 1024), "[" * 1500 + "]" * 1500]:
            with self.subTest(length=len(text)), self.assertRaises(RuntimeError):
                parse_document(text)

    def test_yaml_exception_chain_does_not_expose_original_input(self):
        text = "name: [secret-value"
        try:
            parse_document(text)
        except RuntimeError as error:
            self.assertNotIn("secret-value", "".join(traceback.format_exception(error)))
        else:
            self.fail("Invalid YAML was accepted")

    def test_duplicate_and_non_string_nested_keys(self):
        for text in ["name: {a: 1, a: 2}", "name: {true: value}", "name: {null: value}"]:
            with self.subTest(text=text), self.assertRaises(RuntimeError):
                parse_document(text)


class InputValidatorTests(unittest.TestCase):
    def test_safe_path(self):
        self.assertEqual(safe_path("project//./Dockerfile"), "project/Dockerfile")
        self.assertEqual(safe_path("a b/file"), "a b/file")
        for value in ["", ".", "./", "/absolute", "../x", "a/../x", "a\\b", "a\nb",
                      "a\x00b", "a\x7fb", "C:/file", None, 12]:
            with self.subTest(value=value), self.assertRaises(RuntimeError):
                safe_path(value)

    def test_safe_repositories(self):
        for value in ["https://github.com/team/repo.git", "ssh://git@github.com:2222/team/repo",
                      "git@github.com:team/repo.git"]:
            self.assertEqual(repository(value), value)
        for value in ["https://user:secret-value@github.com/repo", "https://secret-value@host/repo",
                      "ssh://git:secret-value@host/repo", "https://host/repo?secret-value",
                      "https://host/repo#secret-value", "file:///secret-value", "ext::secret-value",
                      "../secret-value", "http://host/repo", "git://host/repo", "https://host",
                      "https://host/repo\nsecret-value", "git@host:../secret-value", None]:
            with self.subTest(value=value), self.assertRaises(RuntimeError) as caught:
                repository(value)
            self.assertNotIn("secret-value", str(caught.exception))

    def test_public_hostnames(self):
        self.assertEqual(public_hostname("api.github.com"), "api.github.com")
        for value in ["localhost", "a.localhost", "a.local", "a.internal", "a.lan", "a.home",
                      "a.test", "a.invalid", "a.example", "a.localdomain", "a.intranet",
                      "127.0.0.1", "[::1]", "*.github.com",
                      "https://github.com", "github.com:443", "bad_.com", "-bad.com", "a..com",
                      "github.com.", "10.0.0.1", "kubernetes.default.svc.cluster.local"]:
            with self.subTest(value=value), self.assertRaises(RuntimeError):
                public_hostname(value)


class PlanTests(unittest.TestCase):
    def test_exact_minimal_plan(self):
        result = planned()
        self.assertEqual(result, {
            "apiVersion": "brood.dev/v1alpha1", "project": "example",
            "target": "development", "environment": "python",
            "sources": {"project": {"type": "project", "destination": "project", "gitMetadata": False}},
            "build": {"dockerfile": "project/environment/Dockerfile", "platform": "linux/amd64", "arguments": {}},
            "actor": minimal()["templates"]["python"]["actor"], "network": {},
            "registry": "localhost:5000/team", "atespace": "development", "pins": {}, "pinsFile": None,
        })
        self.assertEqual(json.loads(json.dumps(result)), result)

    def test_shared_environment_separate_runtime_and_independent_outputs(self):
        document = minimal()
        document["templates"]["coding"] = copy.deepcopy(document["templates"]["python"])
        document["templates"]["coding"]["actor"]["containers"][0]["args"] = ["-i"]
        document["templates"]["coding"]["network"] = "internet"
        document["networks"]["internet"] = {"tls": {"hosts": ["github.com"], "ports": [443]}}
        document["targets"]["coding"] = {"template": "coding"}
        original = copy.deepcopy(document)
        first, second = planned(document), planned(document, target="coding")
        self.assertEqual(first["build"], second["build"])
        self.assertEqual(first["sources"], second["sources"])
        self.assertNotEqual(first["actor"], second["actor"])
        self.assertNotEqual(first["network"], second["network"])
        second["actor"]["containers"][0]["args"].append("changed")
        second["network"]["tls"]["hosts"].append("example.org")
        second["sources"]["project"]["destination"] = "changed"
        self.assertEqual(document, original)
        self.assertEqual(first, planned(document))

    def test_pins_resolved_and_only_selected_pins_returned(self):
        document = minimal()
        document["pinsFile"] = "./pins.json"
        document["sources"]["upstream"] = {
            "type": "git", "destination": "upstream", "repository": "https://github.com/team/repo",
            "revisionPin": "SOURCE_REVISION", "gitMetadata": True,
        }
        document["environments"]["python"]["sources"].append("upstream")
        build = document["environments"]["python"]["build"]
        build["arguments"] = {"MODE": "release"}
        build["argsFromPins"] = ["VERSION"]
        pins = {"SOURCE_REVISION": "a" * 40, "VERSION": "3.12", "UNUSED": "unused"}
        result = planned(document, pins=pins)
        self.assertEqual(result["pins"], {"SOURCE_REVISION": "a" * 40, "VERSION": "3.12"})
        self.assertEqual(result["sources"]["upstream"]["revision"], "a" * 40)
        self.assertNotIn("revisionPin", result["sources"]["upstream"])
        self.assertEqual(result["build"]["arguments"], {"MODE": "release", "VERSION": "3.12"})
        self.assertEqual(result["pinsFile"], "pins.json")
        self.assertIn("revisionPin", document["sources"]["upstream"])
        result["pins"]["VERSION"] = "changed"
        self.assertEqual(pins["VERSION"], "3.12")

    def test_invalid_fields_and_references(self):
        cases = [
            (("defaultTarget",), "missing"),
            (("targets", "development", "template"), "missing"),
            (("templates", "python", "environment"), "missing"),
            (("templates", "python", "network"), "missing"),
            (("environments", "python", "sources"), ["missing"]),
            (("environments", "python", "sources"), ["project", "project"]),
            (("sources", "project", "destination"), "../bad"),
            (("pinsFile",), "../pins.json"),
            (("environments", "python", "build", "dockerfile"), "other/Dockerfile"),
            (("environments", "python", "build", "dockerfile"), "project"),
            (("environments", "python", "build", "arguments"), {"API_TOKEN": "secret-value"}),
            (("templates", "python", "actor", "resources", "limits", "memoryBytes"), str(2**64)),
            (("templates", "python", "actor", "containers", 0, "env"),
             [{"name": "A", "value": "1"}, {"name": "A", "value": "2"}]),
            (("templates", "python", "actor", "volumes"),
             [{"name": "data", "durableDir": {}}, {"name": "data", "durableDir": {}}]),
            (("templates", "python", "actor", "containers", 0, "volumeMounts"),
             [{"name": "missing", "mountPath": "/data"}]),
        ]
        for path, value in cases:
            document = minimal()
            replace(document, path, value)
            with self.subTest(path=path), self.assertRaises(RuntimeError) as caught:
                planned(document, filename="custom.yaml")
            self.assertIn("custom.yaml", str(caught.exception))
            self.assertNotIn("secret-value", str(caught.exception))

    def test_all_unselected_definitions_are_validated(self):
        for section, value in [
            ("targets", {"template": "missing"}),
            ("templates", {"environment": "missing", "network": "offline",
                           "actor": minimal()["templates"]["python"]["actor"]}),
            ("environments", {"sources": ["missing"],
                              "build": {"dockerfile": "project/Dockerfile", "platform": "linux/amd64"}}),
            ("networks", {"tls": {"hosts": ["localhost"], "ports": [443]}}),
            ("sources", {"type": "git", "destination": "other", "revisionPin": "UNKNOWN",
                         "repository": "https://github.com/team/repo"}),
        ]:
            document = minimal()
            document[section]["unused"] = value
            with self.subTest(section=section), self.assertRaises(RuntimeError):
                planned(document)

    def test_source_destinations_do_not_overlap_and_project_is_unique(self):
        for destination in ["project", "project/sub", "./project//sub", "other"]:
            document = minimal()
            document["sources"]["second"] = {"type": "project", "destination": destination}
            with self.subTest(destination=destination), self.assertRaises(RuntimeError):
                planned(document)
        document = minimal()
        document["sources"]["second"] = {"type": "git", "destination": "project/sub",
                                          "repository": "git@github.com:team/repo", "revision": "a" * 40}
        with self.assertRaises(RuntimeError):
            planned(document)

    def test_invalid_pins(self):
        for pins in [[], {"VERSION": 12}, {"VERSION": True}, {"bad": "v1"},
                     {"VERSION": "secret-value\x00"}, {"VERSION": "x" * 16385}]:
            with self.subTest(pins=pins), self.assertRaises(RuntimeError) as caught:
                planned(pins=pins)
            self.assertNotIn("secret-value", str(caught.exception))
        for pins in [None, {}, {"VERSION": "v1"}]:
            document = minimal()
            document["pinsFile"] = "pins.json"
            document["environments"]["python"]["build"]["argsFromPins"] = ["MISSING"]
            with self.subTest(pins=pins), self.assertRaises(RuntimeError):
                planned(document, pins=pins)

    def test_argument_collision_and_secret_pin_names(self):
        for arguments, from_pins in [({"VERSION": "a"}, ["VERSION"]), ({}, ["AUTH_KEY"])]:
            document = minimal()
            document["pinsFile"] = "pins.json"
            document["environments"]["python"]["build"].update(arguments=arguments, argsFromPins=from_pins)
            with self.subTest(arguments=arguments), self.assertRaises(RuntimeError):
                planned(document, pins={"VERSION": "b", "AUTH_KEY": "secret-value"})

    def test_revision_pins_and_pin_file_declaration(self):
        for revision in ["short", "A" * 40, "g" * 40]:
            document = minimal()
            document["pinsFile"] = "pins.json"
            document["sources"]["upstream"] = {
                "type": "git", "destination": "upstream", "revisionPin": "REVISION",
                "repository": "https://github.com/team/repo",
            }
            with self.subTest(revision=revision), self.assertRaises(RuntimeError):
                planned(document, pins={"REVISION": revision})
        document = minimal()
        document["environments"]["python"]["build"]["argsFromPins"] = ["VERSION"]
        with self.assertRaises(RuntimeError):
            planned(document, pins={"VERSION": "1.0"})

    def test_registry_and_atespace_context(self):
        for key, setting in [("registry", "BUILD_REGISTRY"), ("atespace", "BUILD_ATESPACE")]:
            with self.assertRaisesRegex(RuntimeError, setting):
                planned(**{key: None})
        for registry in ["https://host/team", "user:secret-value@host/team", "host/team:tag",
                         "host/team?secret-value", "host/../team", "host//team", "host:99999/team"]:
            with self.subTest(registry=registry), self.assertRaises(RuntimeError) as caught:
                planned(registry=registry)
            self.assertNotIn("secret-value", str(caught.exception))
        for atespace in ["UPPER", "bad.space", "bad_underscore", "a" * 64]:
            with self.subTest(atespace=atespace), self.assertRaises(RuntimeError):
                planned(atespace=atespace)
        with self.assertRaises(RuntimeError):
            planned(target="unknown")

    def test_volume_mounts_valid_and_duplicate_mounts_refused(self):
        document = minimal()
        actor = document["templates"]["python"]["actor"]
        actor["volumes"] = [{"name": "data", "durableDir": {}}]
        container = actor["containers"][0]
        container["volumeMounts"] = [{"name": "data", "mountPath": "/data"}]
        self.assertEqual(planned(document)["actor"], actor)
        container["volumeMounts"].append({"name": "data", "mountPath": "/other"})
        with self.assertRaises(RuntimeError):
            planned(document)
