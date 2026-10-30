"""FEATURE: Чтение статуса и замена pod воркеров своего namespace.

SPEC: Никаких cluster-wide, Secrets, exec и правок pod. Удаление нужно, чтобы
вернуть ёмкость осушенного воркера (#89, #92): RBAC не умеет ограничивать его
меткой, поэтому принадлежность пулу проверяет ReplaceWorkerPod.
"""
import json
from pathlib import Path
import unittest


class WorkerPodAccessTests(unittest.TestCase):
    def test_exact_namespaced_allowlist(self):
        source = Path(__file__).parents[1] / "manifests" / "worker-access.json"
        manifest = json.loads(source.read_text())
        role, binding = manifest["items"]
        self.assertEqual(role["kind"], "Role")
        self.assertEqual(role["metadata"]["namespace"], "brood")
        self.assertEqual(role["rules"], [
            {"apiGroups": ["ate.dev"], "resources": ["workerpools"],
             "verbs": ["get", "list", "create", "update", "delete"]},
            {"apiGroups": [""], "resources": ["pods"],
             "verbs": ["get", "list", "delete"]},
        ])
        self.assertEqual(binding["roleRef"]["kind"], "Role")
        self.assertEqual(binding["subjects"], [
            {"kind": "ServiceAccount", "name": "django", "namespace": "brood"},
        ])
