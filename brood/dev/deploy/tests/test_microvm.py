"""Обратимое переключение пустой установки на отдельный microVM-пул."""
import copy
import json
import unittest
from unittest import mock

from brood.common import cli
from brood.dev.deploy import microvm, server

# FEATURE: Отдельный microVM-пул с остановкой пустого gvisor-пула после готовности.
# SPEC: Владение и KVM до записи; отказ при акторах; новый пул без чужой identity;
# исходный пул не удаляется, его настройки сохраняются, конфликт не обходится.
# APPROACH 1 (ABANDONED): Класс существующего WorkerPool неизменяем в CRD.
# APPROACH 2: Создать brood-microvm, проверить runtime, остановить brood по версии.
# IMPLEMENTATION: pinned assets и ko; проверка регистрации и drain перед остановкой.
# RESULT: IMPLEMENTED — отказ до остановки при акторах, активных воркерах и конфликте.
# STATUS: DONE

IMAGE = "localhost:5002/ateworker-microvm@sha256:" + "b" * 64


def pool():
    result = server.initial_pool("localhost:5002/ateworker-gvisor@sha256:" + "a" * 64)
    result["metadata"].update(uid="pool-uid", resourceVersion="42", labels={"owner": "operator"})
    result["spec"]["template"]["nodeSelector"] = {"kubernetes.io/arch": "amd64"}
    result["status"] = {"readyReplicas": 1}
    return result


class MicroVMManifestTests(unittest.TestCase):
    def test_new_pool_preserves_spec_without_copying_kubernetes_identity(self):
        original = pool()
        before = copy.deepcopy(original)
        changed = microvm.microvm_pool(original, IMAGE)
        self.assertEqual(original, before)
        self.assertNotIn("status", changed)
        self.assertEqual(changed["metadata"], {
            "name": "brood-microvm", "namespace": "brood",
            "labels": {"app.kubernetes.io/managed-by": "brood"},
            "annotations": {"brood.dev/source-pool-uid": "pool-uid"}})
        expected = copy.deepcopy(original["spec"])
        expected.update(workerImage=IMAGE, sandboxClasses=[{
            "name": "microvm", "configRef": {"name": "microvm"}}])
        self.assertEqual(changed["spec"], expected)

    def test_missing_identity_or_wrong_resource_is_refused(self):
        for field in ("uid", "resourceVersion", "name", "namespace"):
            with self.subTest(field=field):
                value = pool()
                del value["metadata"][field]
                with self.assertRaisesRegex(RuntimeError, "WorkerPool"):
                    microvm.microvm_pool(value, IMAGE)
        value = pool()
        value["metadata"]["namespace"] = "other"
        with self.assertRaisesRegex(RuntimeError, "WorkerPool"):
            microvm.microvm_pool(value, IMAGE)

    def test_wrong_or_mutable_worker_image_is_refused(self):
        for image in ("", "localhost:5002/ateworker-microvm:latest",
                      "other/worker@sha256:" + "b" * 64,
                      "localhost:5002/ateworker-gvisor@sha256:" + "b" * 64):
            with self.subTest(image=image), self.assertRaisesRegex(RuntimeError, "worker image"):
                microvm.microvm_pool(pool(), image)

    def test_stopping_preserves_identity_class_image_and_other_settings(self):
        original = pool()
        before = copy.deepcopy(original)
        stopped = microvm.stopped_pool(original)
        self.assertEqual(original, before)
        self.assertNotIn("status", stopped)
        self.assertEqual(stopped["metadata"], original["metadata"])
        expected = copy.deepcopy(original["spec"])
        expected["replicas"] = 0
        self.assertEqual(stopped["spec"], expected)

    def test_empty_actor_response_is_allowed(self):
        for value in ({}, {"actors": []}):
            self.assertIsNone(microvm.require_empty(value))

    def test_any_actor_prevents_migration_including_suspended_actors(self):
        for state in ("ACTOR_STATE_RUNNING", "ACTOR_STATE_SUSPENDED"):
            actor = {"metadata": {"name": "busy"}, "status": {"state": state}}
            with self.subTest(state=state), self.assertRaisesRegex(RuntimeError, "actors.*busy"):
                microvm.require_empty({"actors": [actor]})

    def test_malformed_actor_response_fails_closed(self):
        for value in (None, [], {"actors": None}, {"actors": {}}, {"actors": [None]}):
            with self.subTest(value=value), self.assertRaisesRegex(RuntimeError, "actor"):
                microvm.require_empty(value)

    def test_source_workers_must_be_draining_before_emptiness_can_be_trusted(self):
        source = {"workerNamespace": "brood", "workerPool": "brood", "status": {
            "state": "WORKER_STATE_DRAINING"}}
        self.assertIsNone(microvm.require_drained({"workers": [source]}, 1))
        for response in ({}, {"workers": []}, {"workers": [dict(source, status={
                "state": "WORKER_STATE_ACTIVE"})]}):
            with self.subTest(response=response), self.assertRaisesRegex(RuntimeError, "drain"):
                microvm.require_drained(response, 1)

    def test_readiness_requires_registered_active_microvm_workers_from_target_pool(self):
        worker = {"workerNamespace": "brood", "workerPool": "brood-microvm", "status": {
            "state": "WORKER_STATE_ACTIVE", "sandboxClasses": [{
                "sandboxClass": "microvm", "defaultRuntime": {"version": {"schemaVersion": "v1"}}}]}}
        self.assertTrue(microvm.workers_ready({"workers": [worker]}, 1))
        self.assertFalse(microvm.workers_ready({"workers": [worker]}, 2))
        for key, value in (("workerPool", "brood"), ("workerNamespace", "other")):
            changed = copy.deepcopy(worker)
            changed[key] = value
            self.assertFalse(microvm.workers_ready({"workers": [changed]}, 1))
        for status in ({"state": "WORKER_STATE_DRAINING"},
                       {"state": "WORKER_STATE_ACTIVE", "sandboxClasses": []},
                       {"state": "WORKER_STATE_ACTIVE", "sandboxClasses": [{
                           "sandboxClass": "microvm"}]}):
            changed = dict(worker, status=status)
            self.assertFalse(microvm.workers_ready({"workers": [changed]}, 1))


class MicroVMDeployTests(unittest.TestCase):
    def deployment(self):
        cluster = mock.Mock(source="/substrate")
        cluster.shell.env = {"PATH": "/bin"}
        deploy = microvm.Deploy(cluster)
        deploy._ate = mock.Mock(return_value={"workers": [{
            "workerNamespace": "brood", "workerPool": "brood", "status": {
                "state": "WORKER_STATE_DRAINING"}}]})
        return deploy

    def test_command_is_discovered(self):
        self.assertIn("microvm", cli.groups()["deploy"])

    def test_preflight_rejects_missing_kvm_before_preparing_anything(self):
        deploy = self.deployment()
        namespace = {"metadata": {"labels": {"app.kubernetes.io/managed-by": "brood"}}}
        deploy._json = mock.Mock(side_effect=[namespace, pool(), None])
        deploy._actors = mock.Mock(return_value={})
        deploy._prepare = mock.Mock()
        deploy.shell.output.return_value = (1, "", "")
        with self.assertRaisesRegex(RuntimeError, "KVM"):
            deploy.migrate()
        deploy._prepare.assert_not_called()

    def test_microvm_worker_image_uses_the_same_pinned_ko_and_cache(self):
        deploy = server.Deploy(mock.Mock(source="/substrate"))
        deploy.shell.env = {"PATH": "/bin"}
        shell = mock.Mock()
        shell.output.side_effect = [(0, "amd64\n", ""), (0, "v1\n", ""),
                                    (0, IMAGE + "\n", "")]
        with mock.patch.object(server.local, "Shell", return_value=shell) as factory:
            self.assertEqual(deploy._worker_image("microvm"), IMAGE)
        env, cwd = factory.call_args.args
        self.assertEqual(cwd, "/substrate")
        self.assertEqual(env["KO_DEFAULTPLATFORMS"], "linux/amd64")
        self.assertIn("go.ate.dev/substrate/cmd/ateworker-microvm",
                      shell.output.call_args.args)

    def test_ownership_failure_stops_before_any_command(self):
        deploy = self.deployment()
        deploy.cluster.check.side_effect = RuntimeError("foreign container")
        with self.assertRaisesRegex(RuntimeError, "foreign container"):
            deploy.migrate()
        deploy.shell.output.assert_not_called()
        deploy.shell.run.assert_not_called()

    def test_preflight_failure_does_not_prepare_assets_or_modify_pool(self):
        deploy = self.deployment()
        deploy._preflight = mock.Mock(side_effect=RuntimeError("KVM is not available"))
        deploy._prepare = mock.Mock()
        with self.assertRaisesRegex(RuntimeError, "KVM"):
            deploy.migrate()
        deploy._prepare.assert_not_called()
        deploy.shell.run.assert_not_called()

    def test_target_readiness_failure_leaves_gvisor_pool_running(self):
        deploy = self.deployment()
        deploy._preflight = mock.Mock(return_value=(pool(), None))
        deploy._prepare = mock.Mock(return_value=IMAGE)
        deploy._write = mock.Mock()
        deploy._wait = mock.Mock(side_effect=RuntimeError("runtime not registered"))
        with self.assertRaisesRegex(RuntimeError, "runtime"):
            deploy.migrate()
        self.assertEqual([call.args[0] for call in deploy._write.call_args_list], ["create"])

    def test_active_source_worker_is_never_stopped_even_when_actor_list_is_empty(self):
        deploy = self.deployment()
        deploy._preflight = mock.Mock(return_value=(pool(), None))
        deploy._prepare = mock.Mock(return_value=IMAGE)
        deploy._write = mock.Mock()
        deploy._wait = mock.Mock()
        deploy._actors = mock.Mock(return_value={})
        deploy._wait_stopped = mock.Mock()
        deploy._ate.return_value = {"workers": [{"workerNamespace": "brood", "workerPool": "brood",
                                                "status": {"state": "WORKER_STATE_ACTIVE"}}]}
        with self.assertRaisesRegex(RuntimeError, "drain"):
            deploy.migrate()
        self.assertNotIn("replace", [call.args[0] for call in deploy._write.call_args_list])

    def test_actors_created_during_preparation_prevent_stopping_source(self):
        deploy = self.deployment()
        deploy._preflight = mock.Mock(return_value=(pool(), None))
        deploy._prepare = mock.Mock(return_value=IMAGE)
        deploy._write = mock.Mock()
        deploy._wait = mock.Mock()
        deploy._actors = mock.Mock(return_value={"actors": [{"metadata": {"name": "new"}}]})
        with self.assertRaisesRegex(RuntimeError, "actors.*new"):
            deploy.migrate()
        self.assertNotIn("replace", [call.args[0] for call in deploy._write.call_args_list])

    def test_success_creates_and_checks_target_before_stopping_source(self):
        deploy = self.deployment()
        original = pool()
        deploy._preflight = mock.Mock(return_value=(original, None))
        deploy._prepare = mock.Mock(return_value=IMAGE)
        deploy._actors = mock.Mock(return_value={})
        events = []
        deploy._write = mock.Mock(side_effect=lambda verb, value: events.append((verb, value)))
        deploy._wait = mock.Mock(side_effect=lambda replicas: events.append(("ready", replicas)))
        deploy._wait_stopped = mock.Mock(side_effect=lambda: events.append(("stopped", 0)))
        self.assertEqual(deploy.migrate(), IMAGE)
        self.assertEqual(events, [("create", microvm.microvm_pool(original, IMAGE)),
                                  ("ready", 1), ("replace", microvm.stopped_pool(original)),
                                  ("stopped", 0)])
        deploy.cluster.check.assert_called_once()

    def test_existing_foreign_target_is_refused_before_preparation(self):
        deploy = self.deployment()
        deploy._json = mock.Mock(side_effect=[
            {"metadata": {"labels": {"app.kubernetes.io/managed-by": "brood"}}},
            pool(), {"metadata": {"name": "brood-microvm"}}])
        deploy._prepare = mock.Mock()
        with self.assertRaisesRegex(RuntimeError, "brood-microvm.*belong"):
            deploy.migrate()
        deploy._prepare.assert_not_called()

    def test_conflict_is_not_retried_or_forced(self):
        deploy = self.deployment()
        deploy.shell.output.return_value = (1, "", "Conflict: object has been modified")
        with self.assertRaisesRegex(RuntimeError, "Conflict"):
            deploy._write("replace", microvm.stopped_pool(pool()))
        deploy.shell.output.assert_called_once()
        self.assertNotIn("--force", deploy.shell.output.call_args.args)

    def test_write_serializes_manifest_with_precondition(self):
        deploy = self.deployment()
        manifest = microvm.stopped_pool(pool())

        def output(*args):
            self.assertIn("replace", args)
            with open(args[args.index("-f") + 1]) as f:
                self.assertEqual(json.load(f), manifest)
            return 0, "replaced", ""

        deploy.shell.output.side_effect = output
        deploy._write("replace", manifest)
