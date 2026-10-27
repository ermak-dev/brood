"""Ресурсы используют весь диапазон uint64 protojson."""
import unittest

from brood.dev.build.tests.test_plan import minimal, planned


class ResourceTests(unittest.TestCase):
    def test_maximum_uint64_is_accepted(self):
        document = minimal()
        actor = next(iter(document["templates"].values()))["actor"]
        actor["resources"]["limits"]["memoryBytes"] = str(2**64 - 1)
        result = planned(document)
        self.assertEqual(result["actor"]["resources"]["limits"]["memoryBytes"], str(2**64 - 1))

    def test_unsupported_volume_mount_fields_are_refused(self):
        document = minimal()
        actor = next(iter(document["templates"].values()))["actor"]
        actor["volumes"] = [{"name": "data", "durableDir": {}}]
        actor["containers"][0]["volumeMounts"] = [{"name": "data", "mountPath": "/data", "readOnly": True}]
        with self.assertRaises(RuntimeError):
            planned(document)

    def test_identifier_and_resource_patterns_reject_final_newlines(self):
        for kind in ("project", "argument", "resource"):
            document = minimal()
            if kind == "project":
                document["name"] += "\n"
            elif kind == "argument":
                document["environments"]["python"]["build"]["arguments"] = {"VERSION\n": "1"}
            else:
                next(iter(document["templates"].values()))["actor"]["resources"]["limits"]["memoryBytes"] = "1024\n"
            with self.subTest(kind=kind), self.assertRaises(RuntimeError):
                planned(document)

    def test_uint64_overflow_is_refused(self):
        document = minimal()
        next(iter(document["templates"].values()))["actor"]["resources"]["limits"]["memoryBytes"] = str(2**64)
        with self.assertRaisesRegex(RuntimeError, "uint64"):
            planned(document)
