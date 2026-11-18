"""Проверки выкладки существующей установки без operator credentials."""
import base64
import copy
import io
import json
import os
import subprocess
import sys
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch

from brood.dev.deploy import public
from brood.dev.deploy.go_server import OWNER, manifests
from brood.dev.deploy.tests.test_public import VALUES


IMAGE = "localhost:5002/brood-go@sha256:" + "a" * 64


class IsolatedTestCase(unittest.TestCase):
    """Process lock живёт в каталоге данных пользователя: у каждого теста свой, настоящий не трогаем."""

    def run(self, result=None):
        with tempfile.TemporaryDirectory(prefix="brood-deploy-lock-test-") as data:
            with patch.dict(os.environ, {"XDG_DATA_HOME": data}), patch(
                "socket.getaddrinfo", side_effect=AssertionError("live DNS is forbidden in deploy tests")
            ):
                return super().run(result)

    def track(self, install):
        self.__dict__.setdefault("started", []).append(install)
        self.addCleanup(install.unlock)
        return install

    def operator_clears_lease(self, *dead):
        """Ручная процедура docs/DEPLOY.md: оператор доказал, что прежние процессы мертвы
        (flock снят ядром), и удалил их Lease. Автоматически Lease не снимается никогда."""
        for process in dead or self.__dict__.get("started", []):
            process.unlock()
        self.kube.objects.pop(("lease", "brood-go-deploy"), None)

    def recovered_by_next(self, now):
        """Следующий запуск после ручной процедуры: recover и release (исход - у вызывающего)."""
        self.operator_clears_lease()
        install = self.process(now=now)
        try:
            return install.recover()
        finally:
            install.release()


def resources(**options):
    objects = manifests(IMAGE, {"gvisor": "registry/worker@sha256:" + "b" * 64}, **options)
    result = {}
    for obj in objects:
        obj["metadata"]["uid"] = obj["metadata"]["name"] + "-uid"
        result[obj["kind"].lower(), obj["metadata"]["name"]] = obj
    deploy = result["deployment", "brood-go"]
    deploy["status"] = {"availableReplicas": 1, "updatedReplicas": 1, "observedGeneration": 1}
    deploy["metadata"]["generation"] = 1
    pvc = result["persistentvolumeclaim", "brood-go-data"]
    pvc["spec"]["volumeName"] = "data-volume"
    pvc["status"] = {"phase": "Bound"}
    result["pvc", "brood-go-data"] = pvc
    result["pv", "data-volume"] = {"metadata": {"name": "data-volume", "uid": "volume-uid"},
        "spec": {"claimRef": {"name": "brood-go-data", "namespace": "brood", "uid": pvc["metadata"]["uid"]}}}
    rs = {"metadata": {"name": "brood-go-rs", "namespace": "brood", "uid": "rs-uid", "ownerReferences": [
        {"kind": "Deployment", "uid": deploy["metadata"]["uid"], "controller": True}]}}
    result["replicaset", "brood-go-rs"] = rs
    pod = {"metadata": {"name": "server", "namespace": "brood", "uid": "pod-uid", "ownerReferences": [
        {"kind": "ReplicaSet", "name": "brood-go-rs", "uid": "rs-uid", "controller": True}],
        "labels": {"app.kubernetes.io/managed-by": "brood-go"}},
        "spec": copy.deepcopy(deploy["spec"]["template"]["spec"]),
        "status": {"phase": "Running", "conditions": [{"type": "Ready", "status": "True"}],
                   "containerStatuses": [{"name": "brood", "imageID": IMAGE, "ready": True}]}}
    pod["spec"]["nodeName"] = "owned-node"
    result["pods", "-l=app=brood-go"] = {"items": [pod]}
    for kind in ("deployment", "service"):  # edge существует и принадлежит brood: preflight его проверяет
        result[kind, "edge"] = {"metadata": {"name": "edge", "namespace": "brood", "uid": kind + "-edge-uid",
                                             "labels": dict(OWNER)}}
    return result


LEASE_REPLY = json.dumps({"metadata": {"uid": "lease-uid", "resourceVersion": "1"}})
CONFLICT_RV = ('Error from server (Conflict): Operation cannot be fulfilled on deployments.apps "brood-go": '
               'the object has been modified; please apply your changes to the latest version and try again')
CONFLICT_UID = ('Error from server (Conflict): Operation cannot be fulfilled on deployments.apps "brood-go": '
                'Precondition failed: UID in precondition: {want}, UID in object meta: {have}')


class FakeTime:
    """Монотонные часы процесса: идут только от sleep, поэтому наблюдение аренды мгновенно."""

    def __init__(self):
        self.now = 0.0
        self.on_sleep = None

    def clock(self):
        return self.now

    def sleep(self, seconds):
        self.now += seconds
        if self.on_sleep:
            self.on_sleep()

    def options(self):
        return {"clock": self.clock, "sleep": self.sleep}


class ExistingInstallTests(IsolatedTestCase):
    def helper(self, **options):
        from brood.dev.deploy.existing_install import ExistingInstall
        objects = resources(**options)
        deployment = public.Deployment(Mock(), get={**VALUES, "BROOD_DEPLOY_MODE": "existing"}.get)
        deployment.get_object = Mock(side_effect=lambda kind, name: copy.deepcopy(objects.get((kind, name))))
        deployment.cluster.shell.output.return_value = (0, LEASE_REPLY, "")  # create/patch Lease
        return ExistingInstall(deployment), objects

    def test_mode_is_explicit_and_existing_does_not_need_admin_password(self):
        values = {**VALUES, "BROOD_DEPLOY_MODE": "existing", "BROOD_ADMIN_PASSWORD": ""}
        self.assertEqual(public.settings(values.get)["BROOD_DEPLOY_MODE"], "existing")
        for mode in ("Existing", "typo", "existing\n"):
            with self.subTest(mode=mode), self.assertRaisesRegex(RuntimeError, "BROOD_DEPLOY_MODE"):
                public.settings({**values, "BROOD_DEPLOY_MODE": mode}.get)
        with self.assertRaisesRegex(RuntimeError, "BROOD_ADMIN_PASSWORD"):
            public.settings({**VALUES, "BROOD_ADMIN_PASSWORD": ""}.get)

    def test_preflight_derives_path_and_refuses_missing_foreign_or_unready_runtime(self):
        helper, objects = self.helper()
        state = helper.check()
        self.assertEqual(state["path"], "/data/brood.db")
        self.assertEqual(state["node"], "owned-node")
        cases = [("deployment", "brood-go"), ("service", "brood-go"), ("pvc", "brood-go-data")]
        for key in cases:
            saved = objects.pop(key)
            with self.subTest(key=key), self.assertRaises(RuntimeError):
                helper.check()
            objects[key] = saved
        objects["pods", "-l=app=brood-go"]["items"][0]["metadata"]["ownerReferences"][0]["uid"] = "foreign"
        with self.assertRaisesRegex(RuntimeError, "runtime"):
            helper.check()
        helper.deployment.cluster.shell.output.assert_not_called()

    def test_foreign_volume_and_unsafe_paths_are_refused(self):
        helper, objects = self.helper()
        objects["pv", "data-volume"]["spec"]["claimRef"]["uid"] = "foreign"
        with self.assertRaisesRegex(RuntimeError, "volume"):
            helper.check()
        helper, objects = self.helper()
        container = objects["deployment", "brood-go"]["spec"]["template"]["spec"]["containers"][0]
        next(e for e in container["env"] if e["name"] == "BROOD_DB_PATH")["value"] = "/data/../foreign.db"
        with self.assertRaisesRegex(RuntimeError, "path|configuration"):
            helper.check()

    def test_helper_manifest_is_pinned_bounded_networkless_and_uses_only_owned_pvc(self):
        helper, _ = self.helper()
        state = helper.check()
        pod, policy = helper.manifests(IMAGE, state, "snapshot", "brood-db-test", "/data/.brood-snapshot-test.db")
        spec = pod["spec"]
        self.assertFalse(spec["automountServiceAccountToken"])
        self.assertEqual(spec["restartPolicy"], "Never")
        self.assertLessEqual(spec["activeDeadlineSeconds"], 180)
        self.assertEqual(spec["volumes"], [{"name": "data", "persistentVolumeClaim": {"claimName": "brood-go-data"}}])
        container = spec["containers"][0]
        self.assertEqual(container["image"], IMAGE)
        self.assertEqual(container["command"], ["/brood", "database", "snapshot"])
        self.assertEqual(container["args"], ["--path", "/data/brood.db", "--output", "/data/.brood-snapshot-test.db"])
        self.assertTrue(container["securityContext"]["readOnlyRootFilesystem"])
        self.assertTrue(container["resources"]["limits"])
        self.assertEqual(policy["spec"]["egress"], [])
        self.assertEqual(policy["spec"]["ingress"], [])
        inspect, _ = helper.manifests(IMAGE, state, "inspect", "brood-db-test", None)
        self.assertTrue(inspect["spec"]["containers"][0]["volumeMounts"][0]["readOnly"])
        with self.assertRaisesRegex(RuntimeError, "digest|image"):
            helper.manifests("brood:latest", state, "snapshot", "brood-db-test", "/data/snapshot")

    def test_metadata_is_strict_and_detects_admin_or_user_loss(self):
        helper, _ = self.helper()
        before = {"schema_version": 5, "enabled_admins": 1, "users": 1, "sha256": "a" * 64, "bytes": 4096}
        self.assertEqual(helper.metadata(json.dumps(before), snapshot=True), before)
        for changed in ({**before, "enabled_admins": 0}, {**before, "users": True}, {**before, "sha256": "bad"}):
            with self.subTest(changed=changed), self.assertRaises(RuntimeError):
                helper.metadata(json.dumps(changed), snapshot=True)
        helper.before = before
        helper.schema = {"from": 5, "to": 6}
        helper.execute = Mock(return_value={"schema_version": 6, "enabled_admins": 1, "users": 1})
        helper.inspect(IMAGE)
        helper.execute.return_value = {"schema_version": 6, "enabled_admins": 1, "users": 2}
        with self.assertRaisesRegex(RuntimeError, "user|admin"):
            helper.inspect(IMAGE)
        # Сервер открыл базу в другой версии, чем та, до которой verify довёл snapshot (#274).
        for after, schema in ((5, {"from": 5, "to": 6}), (6, None)):
            with self.subTest(after=after, schema=schema):
                helper.schema = schema
                helper.execute.return_value = {"schema_version": after, "enabled_admins": 1, "users": 1}
                with self.assertRaisesRegex(RuntimeError, "schema version after rollout"):
                    helper.inspect(IMAGE)

    def test_helper_execution_validates_pod_before_logs_and_cleans_with_uid_precondition(self):
        helper, _ = self.helper()
        calls, created = [], {}

        def output(*args):
            from pathlib import Path
            calls.append(args)
            if "create" in args:
                obj = json.loads(Path(args[args.index("-f") + 1]).read_text())
                obj["metadata"]["uid"] = obj["kind"] + "-uid"
                created[obj["kind"].lower()] = obj
                return 0, json.dumps(obj), ""
            if "wait" in args:
                return 0, "", ""
            if "logs" in args:
                return 0, json.dumps({"schema_version": 5, "enabled_admins": 1, "users": 1,
                    "sha256": "a" * 64, "bytes": 4096}), ""
            if "delete" in args:
                options = json.loads(Path(args[args.index("-f") + 1]).read_text())
                self.assertIn(options["preconditions"]["uid"], {"Pod-uid", "NetworkPolicy-uid"})
                self.assertIn("--raw", args)
                route = args[args.index("--raw") + 1]
                self.assertTrue("/pods/" in route or "/networkpolicies/" in route)
                return 0, "", ""
            raise AssertionError(args)

        original = helper.deployment.get_object.side_effect
        def done(kind, name):
            if kind not in created:
                return original(kind, name)
            obj = copy.deepcopy(created[kind])
            if kind == "pod":
                obj["status"] = {"phase": "Succeeded"}
            return obj

        helper.deployment.get_object.side_effect = done
        helper.deployment.cluster.shell.output.side_effect = output
        result = helper.execute(IMAGE, "snapshot")
        self.assertEqual(result["users"], 1)
        self.assertEqual([obj["kind"] for obj in created.values()], ["NetworkPolicy", "Pod"])
        self.assertEqual(sum("delete" in call for call in calls), 2)
        self.assertTrue(helper.snapshot_path.startswith("/data/.brood-snapshot-"))

    # BUG: API опускает readOnly:false и пустые ingress/egress, ломая gate.
    # HYPOTHESIS: сравнение требует сериализованные значения вместо API defaults.
    # SOLUTION: нормализуем только omitted readOnly:false и пустые списки правил policy.
    # RESULT: FIXED — API defaults проходят, read-only и deny-all инварианты сохраняются.
    # STATUS: FIXED
    def test_preflight_accepts_api_omitted_writable_mount_default(self):
        helper, objects = self.helper()
        specs = [objects["deployment", "brood-go"]["spec"]["template"]["spec"],
                 objects["pods", "-l=app=brood-go"]["items"][0]["spec"]]
        for spec in specs:
            spec["containers"][0]["volumeMounts"][0].pop("readOnly")
        self.assertEqual(helper.check()["path"], "/data/brood.db")
        specs[0]["containers"][0]["volumeMounts"][0]["readOnly"] = True
        with self.assertRaisesRegex(RuntimeError, "configuration"):
            helper.check()

    def test_helper_accepts_only_known_api_omitted_defaults(self):
        helper, _ = self.helper()
        helper.state = helper.check()
        for operation in ("snapshot", "inspect"):
            pod, policy = helper.manifests(
                IMAGE, helper.state, operation, "brood-db-test", "/data/.brood-snapshot-test.db")
            for obj in (pod, policy):
                obj["metadata"]["uid"] = "helper-uid"
            actual_policy = copy.deepcopy(policy)
            del actual_policy["spec"]["ingress"]
            del actual_policy["spec"]["egress"]
            with self.subTest(operation=operation, kind="policy"):
                helper.validate(actual_policy, policy, "helper-uid")
            actual_pod = copy.deepcopy(pod)
            del actual_pod["spec"]["containers"][0]["volumeMounts"][0]["readOnly"]
            with self.subTest(operation=operation, kind="pod"):
                if operation == "snapshot":
                    helper.validate(actual_pod, pod, "helper-uid")
                else:
                    with self.assertRaisesRegex(RuntimeError, "configuration"):
                        helper.validate(actual_pod, pod, "helper-uid")
            for key in ("ingress", "egress", "policyTypes"):
                changed = copy.deepcopy(actual_policy)
                changed["spec"][key] = [{}] if key != "policyTypes" else ["Ingress"]
                with self.subTest(operation=operation, key=key), self.assertRaisesRegex(RuntimeError, "configuration"):
                    helper.validate(changed, policy, "helper-uid")
            changed = copy.deepcopy(pod)
            del changed["spec"]["automountServiceAccountToken"]
            with self.assertRaisesRegex(RuntimeError, "configuration"):
                helper.validate(changed, pod, "helper-uid")

    def rejected_helper(self, mutation, *, replacement=False, delete_race=False, wait_failure=False):
        from pathlib import Path
        helper, _ = self.helper()
        created, events = {}, []
        original = helper.deployment.get_object.side_effect

        def get_object(kind, name):
            if kind not in created:
                return original(kind, name)
            obj = copy.deepcopy(created[kind])
            if replacement and kind == "pod":
                obj["metadata"]["uid"] = "replacement"
            return obj

        def output(*args):
            if "create" in args:
                obj = json.loads(Path(args[args.index("-f") + 1]).read_text())
                obj["metadata"]["uid"] = obj["kind"] + "-uid"
                if obj["kind"] == "Pod":
                    if mutation == "env":
                        obj["spec"]["containers"][0]["env"] = [{"name": "TOKEN", "value": "injected"}]
                    else:
                        obj["spec"]["containers"].append({"name": "sidecar", "image": "foreign"})
                created[obj["kind"].lower()] = obj
                return 0, json.dumps(obj), ""
            if "delete" in args:
                route = args[args.index("--raw") + 1]
                kind = "pod" if "/pods/" in route else "networkpolicy"
                options = json.loads(Path(args[args.index("-f") + 1]).read_text())
                self.assertEqual(options["preconditions"], {"uid": created[kind]["metadata"]["uid"]})
                events.append("delete " + kind)
                return (1 if delete_race and kind == "pod" else 0), "", ""
            if "wait" in args and "--for=delete" in args:
                events.append("confirm pod deletion")
                return (1 if wait_failure else 0), "", ""
            raise AssertionError("rejected Pod must not run or expose logs: " + repr(args))

        helper.deployment.get_object.side_effect = get_object
        helper.deployment.cluster.shell.output.side_effect = output
        return helper, events

    # BUG: отклонённый admission-мутированный Pod остаётся вместе с policy.
    # HYPOTHESIS: cleanup повторяет отказавшую проверку spec вместо проверки UID.
    # SOLUTION: cleanup проверяет identity, удаляет по UID, ждёт Pod, затем удаляет policy.
    # RESULT: FIXED — отклонённые Pod удаляются, чужие UID и неподтверждённое удаление блокируют cleanup.
    # STATUS: FIXED
    def test_rejected_helper_is_deleted_before_its_network_policy(self):
        for mutation in ("env", "sidecar"):
            with self.subTest(mutation=mutation):
                helper, events = self.rejected_helper(mutation)
                with self.assertRaisesRegex(RuntimeError, "configuration"):
                    helper.execute(IMAGE, "snapshot")
                self.assertEqual(events, ["delete pod", "confirm pod deletion", "delete networkpolicy"])

    def test_cleanup_never_deletes_foreign_pod_replacement(self):
        helper, events = self.rejected_helper("env", replacement=True)
        with self.assertRaisesRegex(RuntimeError, "identity"):
            helper.execute(IMAGE, "snapshot")
        self.assertEqual(events, [])

    def test_cleanup_keeps_policy_if_pod_uid_changes_during_delete(self):
        helper, events = self.rejected_helper("env", delete_race=True)
        with self.assertRaises(RuntimeError):
            helper.execute(IMAGE, "snapshot")
        self.assertEqual(events, ["delete pod"])

    def test_cleanup_keeps_policy_until_pod_deletion_is_confirmed(self):
        helper, events = self.rejected_helper("env", wait_failure=True)
        with self.assertRaises(RuntimeError):
            helper.execute(IMAGE, "snapshot")
        self.assertEqual(events, ["delete pod", "confirm pod deletion"])

    def test_post_rollout_refuses_wrong_release_image_before_helper(self):
        helper, _ = self.helper()
        helper.before = {"schema_version": 5, "enabled_admins": 1, "users": 1}
        helper.execute = Mock(return_value=helper.before)
        with self.assertRaisesRegex(RuntimeError, "image|release"):
            helper.inspect("localhost:5002/brood-go@sha256:" + "f" * 64)
        helper.execute.assert_not_called()

    def test_runtime_must_have_pulled_the_configured_digest(self):
        helper, objects = self.helper()
        objects["pods", "-l=app=brood-go"]["items"][0]["status"]["containerStatuses"][0]["imageID"] = (
            "registry/foreign@sha256:" + "f" * 64)
        with self.assertRaisesRegex(RuntimeError, "runtime|image"):
            helper.check()

    def test_snapshot_rechecks_ownership_after_helper_before_server_apply(self):
        helper, objects = self.helper()
        helper.state = helper.check()

        def replaced(*args):
            objects["service", "brood-go"]["metadata"]["uid"] = "replacement"
            return {"schema_version": 5, "enabled_admins": 1, "users": 1, "sha256": "a" * 64, "bytes": 4096}

        helper.execute = Mock(side_effect=replaced)
        with self.assertRaisesRegex(RuntimeError, "identity"):
            helper.snapshot(IMAGE)

    def test_helper_rejects_admitted_credentials_privilege_and_volume_changes(self):
        helper, _ = self.helper()
        helper.state = helper.check()
        pod, _ = helper.manifests(IMAGE, helper.state, "inspect", "brood-db-test", None)
        pod["metadata"]["uid"] = "helper-uid"
        for extra in ({"privileged": True}, {"runAsUser": 0}, {"capabilities": {"drop": ["ALL"], "add": ["SYS_ADMIN"]}}):
            changed = copy.deepcopy(pod)
            changed["spec"]["containers"][0]["securityContext"].update(extra)
            with self.subTest(extra=extra), self.assertRaisesRegex(RuntimeError, "configuration"):
                helper.validate(changed, pod, "helper-uid")
        changed = copy.deepcopy(pod)
        changed["spec"]["volumes"].append({"name": "token", "secret": {"secretName": "foreign"}})
        with self.assertRaises(RuntimeError):
            helper.validate(changed, pod, "helper-uid")

    def test_uid_change_between_preflight_and_snapshot_stops_before_mutation(self):
        helper, objects = self.helper()
        helper.state = helper.check()
        objects["pvc", "brood-go-data"]["metadata"]["uid"] = "replacement"
        with self.assertRaisesRegex(RuntimeError, "identity|volume"):
            helper.snapshot(IMAGE)
        helper.deployment.cluster.shell.output.assert_not_called()

    def test_real_go_deploy_calls_backup_with_built_digest_before_apply_and_aborts_on_failure(self):
        from pathlib import Path
        from brood.dev.deploy import go_server

        for fail in (False, True):
            events = []
            get = {**VALUES, "WORKER_POOL_IMAGES": json.dumps({"gvisor": "registry/worker@sha256:" + "b" * 64}),
                   "BROOD_RELEASE_SHA": "c" * 40}.get
            cluster = Mock()

            def output(*args):
                if "build" in args:
                    events.append("build")
                    self.assertIn("VERSION=" + "c" * 40, args)
                    Path(args[args.index("--metadata-file") + 1]).write_text(
                        json.dumps({"containerimage.digest": "sha256:" + "a" * 64}))
                if "apply" in args:
                    events.append("apply")
                return 0, "", ""

            def backup(image):
                self.assertEqual(image, IMAGE)
                events.append("backup")
                if fail:
                    raise RuntimeError("snapshot failed")

            cluster.shell.output.side_effect = output
            deploy = go_server.Deploy(cluster, get=get,
                                      resolver=Mock(return_value=[(2, 1, 6, "", ("149.154.167.220", 443))]))
            with patch.object(go_server, "Infrastructure") as infra, patch.object(deploy, "_registry_access", return_value=None), patch.object(deploy, "publish_bootstrap_artifacts"):
                infra.return_value._tls.return_value = {}
                infra.return_value._api_egress.return_value = []
                if fail:
                    with self.assertRaisesRegex(RuntimeError, "snapshot failed"):
                        deploy.run(before_apply=backup)
                    self.assertEqual(events, ["build", "backup"])
                else:
                    self.assertEqual(deploy.run(before_apply=backup), IMAGE)
                    self.assertEqual(events, ["build", "backup", "apply"])

    # FEATURE: existing-режим переносит BROOD_GIT_REMOTES тем же Secret и rollout.
    # SPEC: snapshot до apply, ключ и secretKeyRef в release, следующий preflight
    # принимает сервер с этой ссылкой; токен не попадает в argv и manifests вне Secret.
    def test_existing_rollout_carries_git_remotes_and_next_preflight_accepts_it(self):
        from pathlib import Path
        from brood.dev.deploy import go_server

        token = "synthetic-existing-git-token"
        remotes = json.dumps({"https://git.example.test/group/repo": {"username": "oauth2", "token": token}})
        get = {**VALUES, "BROOD_DEPLOY_MODE": "existing", "BROOD_GIT_REMOTES": remotes,
               "WORKER_POOL_IMAGES": json.dumps({"gvisor": "registry/worker@sha256:" + "b" * 64})}.get
        events, applied = [], []
        cluster = Mock()

        def output(*args):
            self.assertFalse(any(token in str(arg) for arg in args))
            if "build" in args:
                Path(args[args.index("--metadata-file") + 1]).write_text(
                    json.dumps({"containerimage.digest": "sha256:" + "a" * 64}))
            if "apply" in args:
                events.append("apply")
                applied.extend(json.loads(Path(args[args.index("-f") + 1]).read_text())["items"])
            return 0, "", ""

        cluster.shell.output.side_effect = output
        deploy = go_server.Deploy(cluster, get=get, resolver=Mock(return_value=[(2, 1, 6, "", ("192.168.10.20", 443))]))
        deploy.publish_bootstrap_artifacts = lambda root: None
        with patch.object(go_server, "Infrastructure") as infra, patch.object(deploy, "_registry_access", return_value=None):
            infra.return_value._tls.return_value = {}
            infra.return_value._api_egress.return_value = []
            self.assertEqual(deploy.run(before_apply=lambda image: events.append("backup")), IMAGE)
        self.assertEqual(events, ["backup", "apply"])
        secret = next(o for o in applied if o["metadata"]["name"] == "brood-go-env")
        self.assertEqual(base64.b64decode(secret["data"]["BROOD_GIT_REMOTES"]).decode(), remotes)
        server = next(o for o in applied if o["kind"] == "Deployment")
        env = {e["name"]: e for e in server["spec"]["template"]["spec"]["containers"][0]["env"]}
        self.assertEqual(env["BROOD_GIT_REMOTES"]["valueFrom"]["secretKeyRef"],
                         {"name": "brood-go-env", "key": "BROOD_GIT_REMOTES"})
        self.assertNotIn(token, json.dumps(applied))
        helper, _ = self.helper(git_remotes=True)
        self.assertEqual(helper.check()["path"], "/data/brood.db")

    def test_existing_flow_never_bootstraps_and_reports_pending_operator_acceptance(self):
        values = {**VALUES, "BROOD_DEPLOY_MODE": "existing", "BROOD_ADMIN_PASSWORD": "",
                  "BROOD_DEPLOY_SCALE_FENCE": "live-verified"}
        deploy = public.Deployment(Mock(), get=values.get)
        deploy.check = Mock(return_value=(values, {}))
        deploy.build_edge = Mock(return_value="edge-image")
        deploy.apply_edge = Mock()
        deploy.probe = Mock()
        deploy.bootstrap = Mock(side_effect=AssertionError("must not bootstrap"))
        deploy.cleanup = Mock(side_effect=AssertionError("must not clean legacy data"))
        with patch.object(public, "GoDeploy") as go, patch.object(public, "ExistingInstall") as existing:
            def rollout(*, before_apply):
                before_apply(IMAGE)
                return IMAGE

            go.return_value.run.side_effect = rollout
            result = deploy.run()
            self.assertTrue(callable(go.return_value.run.call_args.kwargs["before_apply"]))
            existing.return_value.snapshot.assert_called_once_with(IMAGE)
            existing.return_value.inspect.assert_called_once_with(IMAGE)
        deploy.probe.assert_called_once_with(values, login=False)
        self.assertEqual(result["acceptance"], "operator-authenticated acceptance pending")


# Дословный вывод kubectl 1.36 на patch с чужим metadata.uid: без префикса 'Error from server'.
INVALID_UID = ('The Deployment "brood-go" is invalid: metadata.uid: Invalid value: '
               '"{want}": field is immutable')

SCHEMA_LOG = ("2026/11/09 serve: open database /data/brood.db: incompatible database schema: "
              "table swarms differs from current schema; recreate the database\n")


def make_crashloop(objects):
    """Единственный pod текущего Deployment в CrashLoopBackOff, rollout не завершён."""
    deploy = objects["deployment", "brood-go"]
    deploy["metadata"]["annotations"] = {"deployment.kubernetes.io/revision": "7"}
    deploy["status"] = {"replicas": 1, "updatedReplicas": 1, "unavailableReplicas": 1, "observedGeneration": 1}
    rs = objects["replicaset", "brood-go-rs"]
    rs["metadata"]["annotations"] = {"deployment.kubernetes.io/revision": "7"}
    rs["spec"] = {"replicas": 1}
    rs["status"] = {"replicas": 1, "readyReplicas": 0}
    objects["replicaset", "-l=app=brood-go"] = {"items": [rs]}
    pod = objects["pods", "-l=app=brood-go"]["items"][0]
    pod["status"] = {"phase": "Running", "conditions": [{"type": "Ready", "status": "False"}],
        "containerStatuses": [{"name": "brood", "imageID": IMAGE, "ready": False, "restartCount": 4,
            "state": {"waiting": {"reason": "CrashLoopBackOff"}},
            "lastState": {"terminated": {"exitCode": 1, "reason": "Error"}}}]}
    return pod


class FakeKubernetes:
    """Состояние кластера и kubectl для orchestration-тестов: настоящие предусловия patch
    (uid, resourceVersion), живые списки pod, исчезновение pod после scale, журнал событий."""

    def __init__(self, objects, log=SCHEMA_LOG):
        self.objects, self.log = objects, log
        self.events, self.created, self.deleted = [], {}, []
        self.extra_pods, self.resource_version = [], 100
        self.objects["deployment", "brood-go"]["metadata"]["resourceVersion"] = "100"
        self.fail_scale = False      # patch отвечает ошибкой, не применившись
        self.fail_scale_after = 0    # сколько первых patch применяются, но ответ потерян
        self.on_create = None        # хук: вызывается при создании helper
        self.after_scale = None      # хук: вызывается после применённого scale to zero
        self.crash = copy.deepcopy(objects["pods", "-l=app=brood-go"]["items"][0])
        self.linger = False          # pod не исчезает после scale
        self.on_patch = None         # хук гонки: вызывается до проверки предусловий
        self.fail_op = None          # helper-операция, у которой logs падает
        self.refuse = {}             # helper-операция -> лог: pod завершается Failed, exit 1
        self.hold_scale = False      # scale(0) зависает в сети и применится позже (deliver_late)
        self.late = []
        self.lose_fence = 0          # сколько fence-записей применены, а ответ потерян
        self.lose_restore = 0        # сколько снятий записи/restore применены, а ответ потерян
        self.drop_intent = 0         # сколько записей намерения отклоняются без применения
        self.lose_intent = 0         # сколько записей намерения применены, а ответ потерян
        self.fail_get_pods = False
        self.ignore_preconditions = False
        self.lease_rv = 500
        self.lease_events = []
        self.prune_reply = None      # ответ helper хранения снимков (#247); None - честный
        self.schema = (16, 16)       # версия snapshot и цель verify (#274)
        self.after_version = None    # версия базы после rollout; None - цель verify

    def bump(self):
        self.resource_version += 1
        self.objects["deployment", "brood-go"]["metadata"]["resourceVersion"] = str(self.resource_version)

    def brood_pods(self):
        return self.objects["pods", "-l=app=brood-go"]["items"]

    def get_object(self, kind, name):
        if kind in self.created and name.startswith("brood-db-"):
            obj = copy.deepcopy(self.created[kind])
            if kind == "pod" and obj["spec"]["containers"][0]["command"][2] in self.refuse:
                obj["status"] = {"phase": "Failed", "containerStatuses": [
                    {"name": "database", "state": {"terminated": {"reason": "Error", "exitCode": 1}}}]}
            elif kind == "pod":
                obj["status"] = {"phase": "Succeeded"}
            return obj
        if kind == "pod":
            return copy.deepcopy(next((p for p in [*self.brood_pods(), *self.extra_pods]
                                       if p["metadata"]["name"] == name), None))
        if (kind, name) == ("pods", "-l=app=brood-go"):
            return copy.deepcopy(self.objects[kind, name])
        return copy.deepcopy(self.objects.get((kind, name)))

    def apply_patch(self, body):
        """Модель предусловий и generation настоящего apiserver 1.36 для Deployment: uid и
        resourceVersion сверяются в момент записи; generation растёт при любой смене spec ИЛИ
        metadata.annotations (стратегия Deployment); запись, ничего не меняющая, не создаёт
        ни новой resourceVersion, ни нового generation (снятие отсутствующей аннотации)."""
        deploy = self.objects["deployment", "brood-go"]
        meta = deploy["metadata"]
        pre = body["metadata"]
        if pre.get("uid") != meta["uid"] or pre.get("resourceVersion") != meta["resourceVersion"]:
            return False
        before_notes = dict(meta.get("annotations", {}))
        notes = dict(before_notes)
        for key, value in pre.get("annotations", {}).items():
            if value is None:
                notes.pop(key, None)
            else:
                notes[key] = value
        replicas = body.get("spec", {}).get("replicas")
        changed_spec = replicas is not None and replicas != deploy["spec"]["replicas"]
        if notes == before_notes and not changed_spec:
            return True  # no-op: etcd не пишется, RV и generation прежние
        meta["annotations"] = notes
        meta["generation"] += 1
        deploy["status"]["observedGeneration"] = meta["generation"]  # контроллер догоняет сам
        if changed_spec:
            deploy["spec"]["replicas"] = replicas
            if replicas == 0 and not self.linger:
                self.brood_pods().clear()
            if replicas == 1 and not self.brood_pods():
                # Контроллер поднимает прежний CrashLoop pod заново, уже с новым UID.
                self.crash["metadata"]["uid"] += "+"
                self.brood_pods().append(copy.deepcopy(self.crash))
        self.bump()
        return True

    def deliver_late(self):
        """Запоздавший patch доходит до API: применяется, только если предусловия ещё верны."""
        return [self.apply_patch(body) for body in [self.late.pop(0) for _ in list(self.late)]]

    def lease_patch(self, args):
        lease = self.objects.get(("lease", "brood-go-deploy"))
        body = json.loads(args[args.index("-p") + 1])
        pre = body["metadata"]
        self.lease_events.append("patch")
        if not lease or pre["uid"] != lease["metadata"]["uid"] or pre["resourceVersion"] != lease["metadata"]["resourceVersion"]:
            return 1, "", "Operation cannot be fulfilled on leases.coordination.k8s.io: the object has been modified"
        lease["spec"].update(body["spec"])
        self.lease_rv += 1
        lease["metadata"]["resourceVersion"] = str(self.lease_rv)
        return 0, json.dumps(copy.deepcopy(lease)), ""

    def patch(self, args):
        if "lease" in args:
            return self.lease_patch(args)
        if self.on_patch:
            self.on_patch(self)
        body = json.loads(args[args.index("-p") + 1])
        if "--dry-run=server" in args:
            # Проба предусловий: ничего не пишет, отказывает как настоящий API.
            self.events.append(("probe",))
            meta = self.objects["deployment", "brood-go"]["metadata"]
            if self.ignore_preconditions:
                return 0, json.dumps(self.objects["deployment", "brood-go"]), ""
            if body["metadata"]["uid"] != meta["uid"]:
                return 1, "", INVALID_UID.format(want=body["metadata"]["uid"])
            if body["metadata"]["resourceVersion"] != meta["resourceVersion"]:
                return 1, "", CONFLICT_RV
            return 0, json.dumps(self.objects["deployment", "brood-go"]), ""
        replicas = body.get("spec", {}).get("replicas")
        notes = body["metadata"].get("annotations", {})
        writes = any(v is not None for v in notes.values())
        self.events.append(("patch", replicas) if replicas is not None else ("annotate",))
        if replicas == 0 and self.hold_scale:
            self.late.append(body)  # запрос завис в сети и дойдёт позже
            return 1, "", "request timeout"
        if replicas == 0 and self.fail_scale:
            return 1, "", "boom"
        if replicas is None and writes and self.drop_intent > 0:
            self.drop_intent -= 1
            return 1, "", "connection refused"
        if not self.apply_patch(body):
            return 1, "", "Operation cannot be fulfilled: the object has been modified"
        deploy = self.objects["deployment", "brood-go"]
        if replicas == 0 and self.after_scale:
            self.after_scale(self)
        if replicas == 0 and self.fail_scale_after > 0:
            self.fail_scale_after -= 1
            return 1, "", "connection lost"
        removal = any(v is None for v in notes.values())
        if removal and self.lose_restore > 0:
            self.lose_restore -= 1
            return 1, "", "connection lost"
        fenced = writes and json.loads(next(v for v in notes.values() if v is not None)).get("fence", 0) > 0
        if replicas is None and fenced and self.lose_fence > 0:
            self.lose_fence -= 1
            return 1, "", "connection lost"
        if replicas is None and writes and not fenced and self.lose_intent > 0:
            self.lose_intent -= 1
            return 1, "", "connection lost"
        return 0, json.dumps(copy.deepcopy(deploy)), ""

    def output(self, *args):
        from pathlib import Path
        if "patch" in args:
            return self.patch(args)
        if "create" in args:
            obj = json.loads(Path(args[args.index("-f") + 1]).read_text())
            if obj["kind"] == "Lease":
                if ("lease", obj["metadata"]["name"]) in self.objects:
                    return 1, "", "AlreadyExists"
                self.lease_rv += 1
                obj["metadata"].update(uid="lease-uid-%d" % self.lease_rv, resourceVersion=str(self.lease_rv))
                self.objects["lease", obj["metadata"]["name"]] = obj
                self.lease_events.append("create")
                return 0, json.dumps(copy.deepcopy(obj)), ""
            obj["metadata"]["uid"] = obj["kind"] + "-uid"
            self.created[obj["kind"].lower()] = obj
            self.events.append(("create", obj["kind"]))
            if self.on_create and obj["kind"] == "Pod":
                self.on_create(self)
            return 0, json.dumps(obj), ""
        if "delete" in args and "/leases/" in args[args.index("--raw") + 1]:
            self.objects.pop(("lease", "brood-go-deploy"), None)
            self.lease_events.append("delete")
            return 0, "", ""
        if "delete" in args:
            self.deleted.append(args[args.index("--raw") + 1])
            return 0, "", ""
        if "wait" in args:
            return 0, "", ""
        if "get" in args and "pods" in args and self.fail_get_pods:
            return 1, "", "forbidden"
        if "get" in args and "pods" in args:
            pods = [*self.brood_pods(), *self.extra_pods]
            return 0, json.dumps({"items": pods}), ""
        if "logs" in args and "--previous" in args:
            return 0, self.log, ""
        if "logs" in args:
            op = self.created["pod"]["spec"]["containers"][0]["command"][2]
            if op == self.fail_op:
                return 1, "", ""
            self.events.append(("logs", op))
            if op in self.refuse:
                return 0, self.refuse[op], ""
            if op == "verify":
                found, target = self.schema
                return 0, json.dumps({"schema_version": found, "target_version": target, "verified": True}), ""
            if op == "prune":
                args = self.created["pod"]["spec"]["containers"][0]["args"]
                reply = self.prune_reply or {"current": Path(args[1]).name, "kept": int(args[3]), "removed": 2}
                return 0, json.dumps(reply), ""
            version = self.schema[0] if op == "snapshot" else self.after_version or self.schema[1]
            data = {"schema_version": version, "enabled_admins": 1, "users": 2}
            if op == "snapshot":
                data.update(sha256="a" * 64, bytes=4096)
            return 0, json.dumps(data), ""
        raise AssertionError(args)

    def foreign_pod(self, phase="Running", claim="brood-go-data", name="foreign", uid="foreign-uid"):
        pod = {"metadata": {"name": name, "namespace": "brood", "uid": uid},
               "spec": {"volumes": [{"name": "x", "persistentVolumeClaim": {"claimName": claim}}]},
               "status": {"phase": phase}}
        self.extra_pods.append(pod)
        return pod


# FEATURE: Recovery of an interrupted rollout (#220): a crash-looping current pod
# no longer blocks the next release, but only under a closed set of proofs.
# SPEC: exactly one owned pod of the current Deployment revision, exact UID chain,
# digest, CrashLoopBackOff with exit 1 and a schema-incompatibility previous log;
# verified snapshot and recovery-image schema verification before any server apply.
# The failed Open may have touched SQLite: nothing claims the file is unchanged.
# STATUS: DONE
class CrashLoopRecoveryTests(IsolatedTestCase):
    def helper(self, log=SCHEMA_LOG, log_code=0, **clock):
        from brood.dev.deploy.existing_install import ExistingInstall
        objects = resources()
        make_crashloop(objects)
        self.cluster = FakeKubernetes(objects, log=log)
        deployment = public.Deployment(Mock(), get={**VALUES, "BROOD_DEPLOY_MODE": "existing",
                                                    "BROOD_DEPLOY_SCALE_FENCE": "live-verified"}.get)
        deployment.get_object = Mock(side_effect=self.cluster.get_object)
        deployment.cluster.shell.output.side_effect = (
            lambda *args: (log_code, log, "") if "--previous" in args else self.cluster.output(*args))
        return ExistingInstall(deployment, **clock), objects

    def test_unique_owned_crashloop_pod_is_accepted_as_recovery(self):
        helper, _ = self.helper()
        state = helper.check()
        self.assertEqual(state["mode"], "recovery")
        self.assertEqual(state["path"], "/data/brood.db")
        self.assertEqual(state["recovery"]["pod"], "pod-uid")
        self.assertEqual(state["recovery"]["replicaset"], "rs-uid")
        command = helper.deployment.cluster.shell.output.call_args.args
        self.assertIn("--previous", command)
        self.assertIn("logs", command)

    def test_ready_runtime_is_unchanged_and_never_reads_logs(self):
        from brood.dev.deploy.existing_install import ExistingInstall
        objects = resources()
        deployment = public.Deployment(Mock(), get={**VALUES, "BROOD_DEPLOY_MODE": "existing"}.get)
        deployment.get_object = Mock(side_effect=lambda kind, name: copy.deepcopy(objects.get((kind, name))))
        state = ExistingInstall(deployment).check()
        self.assertNotIn("mode", state)
        deployment.cluster.shell.output.assert_not_called()

    def test_every_uncertain_or_foreign_shape_is_refused(self):
        def second_pod(o, pod):
            o["pods", "-l=app=brood-go"]["items"].append(copy.deepcopy(pod))

        def status(o, pod, **change):
            pod["status"]["containerStatuses"][0].update(change)

        cases = {
            "two pods": second_pod,
            "terminating pod": lambda o, pod: o["pods", "-l=app=brood-go"]["items"].append(
                {**copy.deepcopy(pod), "metadata": {**pod["metadata"], "uid": "old", "deletionTimestamp": "2026-01-01T00:00:00Z"}}),
            "failed pod": lambda o, pod: o["pods", "-l=app=brood-go"]["items"].append(
                {**copy.deepcopy(pod), "metadata": {**pod["metadata"], "uid": "old"}, "status": {"phase": "Failed"}}),
            "foreign replicaset owner": lambda o, pod: o["replicaset", "brood-go-rs"]["metadata"]["ownerReferences"][0].update(uid="foreign"),
            "foreign pod owner": lambda o, pod: pod["metadata"]["ownerReferences"][0].update(uid="foreign"),
            "foreign pod label": lambda o, pod: pod["metadata"]["labels"].update({"app.kubernetes.io/managed-by": "x"}),
            "wrong digest": lambda o, pod: status(o, pod, imageID="registry/foreign@sha256:" + "f" * 64),
            "oom": lambda o, pod: status(o, pod, lastState={"terminated": {"exitCode": 137, "reason": "OOMKilled"}}),
            "exit code": lambda o, pod: status(o, pod, lastState={"terminated": {"exitCode": 2, "reason": "Error"}}),
            "not crashloop": lambda o, pod: status(o, pod, state={"waiting": {"reason": "ImagePullBackOff"}}),
            "running not ready": lambda o, pod: status(o, pod, state={"running": {}}),
            "never restarted": lambda o, pod: status(o, pod, restartCount=0),
            "bool restarts": lambda o, pod: status(o, pod, restartCount=True),
            "no last state": lambda o, pod: status(o, pod, lastState={}),
            "ready condition": lambda o, pod: pod["status"].update(conditions=[{"type": "Ready", "status": "True"}]),
            "pending phase": lambda o, pod: pod["status"].update(phase="Pending"),
            "other replicaset has pods": lambda o, pod: o["replicaset", "-l=app=brood-go"]["items"].append(
                {"metadata": {"name": "old", "uid": "old-uid"}, "spec": {"replicas": 1}, "status": {"replicas": 1}}),
            "replicaset revision": lambda o, pod: o["replicaset", "brood-go-rs"]["metadata"]["annotations"].update(
                {"deployment.kubernetes.io/revision": "6"}),
            "no revision": lambda o, pod: o["deployment", "brood-go"]["metadata"].update(annotations={}),
            "available replica": lambda o, pod: o["deployment", "brood-go"]["status"].update(availableReplicas=1),
            "stale generation": lambda o, pod: o["deployment", "brood-go"]["metadata"].update(generation=2),
            "pod image": lambda o, pod: pod["spec"]["containers"][0].update(image="localhost:5002/brood-go@sha256:" + "d" * 64),
            "foreign pv": lambda o, pod: o["pv", "data-volume"]["spec"]["claimRef"].update(uid="foreign"),
            "no node": lambda o, pod: pod["spec"].pop("nodeName"),
        }
        for name, mutate in cases.items():
            with self.subTest(name):
                helper, objects = self.helper()
                mutate(objects, objects["pods", "-l=app=brood-go"]["items"][0])
                with self.assertRaises(RuntimeError):
                    helper.check()

    def test_other_crash_causes_and_unreadable_logs_are_refused_without_echoing_them(self):
        secret = "synthetic-private-token-123"
        for name, log, code in (
                ("panic", f"panic: nil map {secret}\n", 0),
                ("config", f"BROOD_SESSION_SECRET={secret} is too short\n", 0),
                ("empty", "", 0),
                ("unavailable", SCHEMA_LOG, 1)):
            with self.subTest(name):
                helper, _ = self.helper(log=log, log_code=code)
                with self.assertRaises(RuntimeError) as caught:
                    helper.check()
                self.assertNotIn(secret, str(caught.exception))

    def test_database_path_and_ownership_checks_still_apply_in_recovery(self):
        helper, objects = self.helper()
        container = objects["deployment", "brood-go"]["spec"]["template"]["spec"]["containers"][0]
        next(e for e in container["env"] if e["name"] == "BROOD_DB_PATH")["value"] = "/data/other.db"
        with self.assertRaises(RuntimeError):
            helper.check()
        helper, objects = self.helper()
        del objects["pvc", "brood-go-data"]
        with self.assertRaises(RuntimeError):
            helper.check()

    def test_pod_replacement_between_stages_is_an_identity_change(self):
        helper, objects = self.helper()
        helper.state = helper.check()
        objects["pods", "-l=app=brood-go"]["items"][0]["metadata"]["uid"] = "replacement"
        with self.assertRaisesRegex(RuntimeError, "identity"):
            helper.recheck()
        helper, objects = self.helper()
        helper.state = helper.check()
        objects["pods", "-l=app=brood-go"]["items"][0]["status"]["containerStatuses"][0]["restartCount"] = 9
        helper.recheck()  # restartCount растёт сам: это не смена идентичности

    def test_verify_manifest_reads_only_the_snapshot_and_is_bound_to_its_hash(self):
        helper, _ = self.helper()
        state = helper.check()
        snapshot = "/data/.brood-snapshot-test.db"
        pod, policy = helper.manifests(IMAGE, state, "verify", "brood-db-test", snapshot, sha256="a" * 64)
        container = pod["spec"]["containers"][0]
        self.assertEqual(container["command"], ["/brood", "database", "verify"])
        self.assertEqual(container["args"], ["--path", snapshot, "--sha256", "a" * 64])
        self.assertTrue(container["volumeMounts"][0]["readOnly"])
        self.assertEqual(policy["spec"]["egress"], [])
        for bad in (None, "A" * 64, "a" * 63):
            with self.subTest(bad=bad), self.assertRaisesRegex(RuntimeError, "sha256|hash"):
                helper.manifests(IMAGE, state, "verify", "brood-db-test", snapshot, sha256=bad)
        with self.assertRaisesRegex(RuntimeError, "snapshot"):
            helper.manifests(IMAGE, state, "verify", "brood-db-test", "/data/brood.db", sha256="a" * 64)

    def test_verification_metadata_is_strict(self):
        helper, _ = self.helper()
        good = {"schema_version": 16, "target_version": 16, "verified": True}
        self.assertEqual(helper.verification(json.dumps(good)), good)
        # Миграция (#274): snapshot старше релиза, релиз доводит его до своей версии.
        migrated = {"schema_version": 26, "target_version": 27, "verified": True}
        self.assertEqual(helper.verification(json.dumps(migrated)), migrated)
        for bad in ({"schema_version": 16, "target_version": 16, "verified": False}, {"schema_version": 16},
                    {"schema_version": 16, "verified": True}, {"schema_version": 27, "target_version": 26, "verified": True},
                    {"schema_version": 26, "target_version": "27", "verified": True},
                    {"schema_version": "16", "target_version": 16, "verified": True}, {**good, "extra": 1}, [], 5):
            with self.subTest(bad=bad), self.assertRaises(RuntimeError):
                helper.verification(json.dumps(bad))
        with self.assertRaises(RuntimeError):
            helper.verification("not json")

    def snapshot_flow(self, verified=None, snapshot_error=None, mutate_after=None):
        helper, objects = self.helper()
        helper.state = helper.check()
        events = []
        before = {"schema_version": 16, "enabled_admins": 1, "users": 2, "sha256": "a" * 64, "bytes": 4096}

        def execute(image, operation, **kwargs):
            events.append(operation)
            if operation == "snapshot":
                helper.snapshot_path = "/data/.brood-snapshot-x.db"
                if snapshot_error:
                    raise RuntimeError(snapshot_error)
                return before
            self.assertEqual(helper.before, before)
            if mutate_after:
                mutate_after(objects)
            return verified if verified is not None else {"schema_version": 16, "target_version": 16, "verified": True}

        helper.execute = Mock(side_effect=execute)
        return helper, events

    def test_recovery_snapshot_then_verify_then_apply_order(self):
        helper, events = self.snapshot_flow()
        helper.snapshot(IMAGE)
        self.assertEqual(events, ["snapshot", "verify"])

    def test_failed_snapshot_never_reaches_verify_or_apply(self):
        helper, events = self.snapshot_flow(snapshot_error="snapshot failed")
        with self.assertRaisesRegex(RuntimeError, "snapshot failed"):
            helper.snapshot(IMAGE)
        self.assertEqual(events, ["snapshot"])

    def test_unverified_or_different_version_refuses_rollout(self):
        for verified in ({"schema_version": 15, "target_version": 16, "verified": True}, {"schema_version": 16, "target_version": 16, "verified": False}):
            with self.subTest(verified=verified):
                helper, _ = self.snapshot_flow(verified=verified)
                with self.assertRaises(RuntimeError):
                    helper.snapshot(IMAGE)

    def test_identity_change_during_verify_refuses_rollout(self):
        helper, _ = self.snapshot_flow(mutate_after=lambda o: o["service", "brood-go"]["metadata"].update(uid="replacement"))
        with self.assertRaisesRegex(RuntimeError, "identity"):
            helper.snapshot(IMAGE)

    def ready_snapshot_flow(self, verified):
        """Готовый runtime без opt-in scale: snapshot и verify через подменённый execute."""
        from brood.dev.deploy.existing_install import ExistingInstall
        objects = resources()
        deployment = public.Deployment(Mock(), get={**VALUES, "BROOD_DEPLOY_MODE": "existing"}.get)
        deployment.get_object = Mock(side_effect=lambda kind, name: copy.deepcopy(objects.get((kind, name))))
        deployment.cluster.shell.output.return_value = (0, LEASE_REPLY, "")
        helper = ExistingInstall(deployment)
        helper.state = helper.check()
        operations = []
        before = {"schema_version": 16, "enabled_admins": 1, "users": 1, "sha256": "a" * 64, "bytes": 1}

        def execute(image, operation, **kw):
            operations.append(operation)
            if operation == "snapshot":
                return before
            self.assertEqual(helper.before, before)  # verify связан с хешем именно этого snapshot
            return verified
        helper.execute = Mock(side_effect=execute)
        return helper, operations

    # FEATURE: Schema gate on the healthy existing path (#220).
    # SPEC: a ready runtime is no longer exempt: after the snapshot and before the
    # server apply, the new image verifies that snapshot, with or without the
    # scale-to-zero opt-in; a negative or mismatched verdict refuses the release.
    # STATUS: DONE
    def test_ready_runtime_verifies_the_snapshot_before_apply(self):
        helper, operations = self.ready_snapshot_flow({"schema_version": 16, "target_version": 16, "verified": True})
        helper.snapshot(IMAGE)
        self.assertEqual(operations, ["snapshot", "verify"])
        self.assertEqual(helper.schema, {"from": 16, "to": 16})
        self.assertTrue(helper.handed_off)
        self.assertFalse(helper.recovered)
        self.assertIsNone(helper.quiesced)

    def test_ready_runtime_refuses_unverified_or_other_version_before_apply(self):
        for verified in ({"schema_version": 15, "target_version": 16, "verified": True}, {"schema_version": 16, "target_version": 16, "verified": False}):
            with self.subTest(verified=verified):
                helper, operations = self.ready_snapshot_flow(verified)
                with self.assertRaisesRegex(RuntimeError, "refusing rollout"):
                    helper.snapshot(IMAGE)
                self.assertEqual(operations, ["snapshot", "verify"])
                self.assertFalse(helper.handed_off)
                self.assertIsNone(helper.lease)

    def test_cli_prints_recovery_notice_without_secrets(self):
        import io
        command = __import__("brood.dev.deploy.cli.server", fromlist=["main"])
        result = {"image": IMAGE, "host": "h.example", "acceptance": "pending", "snapshot": "/data/.brood-snapshot-x.db",
                  "recovered": "crashloop"}
        with patch.object(command, "settings"), patch.object(command, "cluster"), \
                patch.object(command, "Deployment") as deploy, patch("sys.stdout", new_callable=io.StringIO) as out:
            deploy.return_value.run.return_value = result
            command.main([])
        self.assertIn("Recovered an interrupted rollout", out.getvalue())

    def test_cli_names_the_schema_migration_of_the_rollout(self):
        import io
        command = __import__("brood.dev.deploy.cli.server", fromlist=["main"])
        for schema, line in (({"from": 26, "to": 27}, "Database schema migrated from 26 to 27 on server start"),
                             ({"from": 27, "to": 27}, "Database schema 27: no migration")):
            with self.subTest(schema=schema):
                result = {"image": IMAGE, "host": "h.example", "acceptance": "pending",
                          "snapshot": "/data/.brood-snapshot-x.db", "schema": schema}
                with patch.object(command, "settings"), patch.object(command, "cluster"), \
                        patch.object(command, "Deployment") as deploy, patch("sys.stdout", new_callable=io.StringIO) as out:
                    deploy.return_value.run.return_value = result
                    command.main([])
                self.assertIn(line, out.getvalue())

    def real_flow(self, healthy=False, trace=None):
        """Согласованный поток: настоящие ExistingInstall и Deployment.run, фейк только kubectl/API."""
        objects = resources()
        if not healthy:
            make_crashloop(objects)
        kube = FakeKubernetes(objects)
        self.cluster = kube
        values = {**VALUES, "BROOD_DEPLOY_MODE": "existing", "BROOD_ADMIN_PASSWORD": "",
                  "BROOD_DEPLOY_SCALE_FENCE": "live-verified"}
        deploy = public.Deployment(Mock(), get=values.get)
        deploy.check = Mock(return_value=(values, {}))
        deploy.build_edge = Mock(return_value="edge-image")
        deploy.apply_edge = Mock()
        deploy.probe = Mock()
        deploy.get_object = Mock(side_effect=kube.get_object)
        deploy.cluster.shell.output.side_effect = kube.output

        def rollout(*, before_apply):
            before_apply(IMAGE)
            kube.events.append(("apply",))
            fresh = resources()  # rollout завершён: тот же владелец, новый готовый pod
            for key in ("deployment", "service", "pvc", "pv"):
                for k in [k for k in objects if k[0] == key]:
                    fresh[k]["metadata"]["uid"] = objects[k]["metadata"]["uid"]
            objects.update({k: v for k, v in fresh.items() if k[0] in {"pods", "replicaset"}})
            deployment = objects["deployment", "brood-go"]
            deployment["status"] = fresh["deployment", "brood-go"]["status"]
            deployment["spec"]["replicas"] = 1
            deployment["metadata"]["generation"] += 1
            deployment["status"]["observedGeneration"] = deployment["metadata"]["generation"]
            kube.bump()
            kube.created.clear()
            return IMAGE

        return deploy, rollout, kube

    def test_real_recovery_flow_reports_recovered_after_inspect_rebase(self):
        deploy, rollout, kube = self.real_flow()
        with patch.object(public, "GoDeploy") as go:
            go.return_value.run.side_effect = rollout
            result = deploy.run()
        self.assertEqual(result["recovered"], "crashloop")
        self.assertEqual(result["acceptance"], "operator-authenticated acceptance pending")
        # snapshot, verify, inspect: policy и pod каждого helper удалены по UID.
        self.assertEqual(len(kube.deleted), 6)
        # Писатель погашен до первого helper; apply - строго после snapshot и verify.
        kinds = [e for e in kube.events if e[0] in {"patch", "logs", "apply"}]
        self.assertEqual(kinds, [("patch", 0), ("logs", "snapshot"), ("logs", "verify"), ("apply",), ("logs", "inspect")])

    def test_healthy_real_flow_never_reports_recovery(self):
        deploy, rollout, kube = self.real_flow(healthy=True)
        with patch.object(public, "GoDeploy") as go:
            go.return_value.run.side_effect = rollout
            result = deploy.run()
        self.assertNotIn("recovered", result)
        self.assertEqual(len(kube.deleted), 6)  # snapshot, verify и inspect: policy и pod каждого
        kinds = [e for e in kube.events if e[0] in {"logs", "apply"}]
        self.assertEqual(kinds, [("logs", "snapshot"), ("logs", "verify"), ("apply",), ("logs", "inspect")])
        # Живой healthy сервер не гасится: прежний протокол без scale.
        self.assertNotIn("patch", [e[0] for e in kube.events])

    def test_post_rollout_inspect_rebases_recovery_state_but_keeps_owned_identities(self):
        helper, objects = self.helper()
        helper.state = helper.check()
        helper.before = {"schema_version": 16, "enabled_admins": 1, "users": 2}
        # Rollout завершён: тот же Deployment, PVC и PV, новый готовый pod.
        fresh = resources()
        for key in objects:
            if key in fresh and key[0] in {"deployment", "service", "pvc", "pv"}:
                fresh[key]["metadata"]["uid"] = objects[key]["metadata"]["uid"]
        objects.update({k: v for k, v in fresh.items() if k[0] in {"pods", "replicaset"}})
        objects["deployment", "brood-go"]["status"] = fresh["deployment", "brood-go"]["status"]
        helper.command = Mock()
        with patch.object(type(helper), "manifests", side_effect=RuntimeError("stop")):
            with self.assertRaisesRegex(RuntimeError, "stop"):
                helper.inspect(IMAGE)  # recheck внутри execute не должен ругаться на identity
        self.assertIsNone(helper.state.get("mode"))
        # Подмена PVC во время восстановления - отказ.
        helper, objects = self.helper()
        helper.state = helper.check()
        helper.before = {"schema_version": 16, "enabled_admins": 1, "users": 2}
        fresh = resources()
        objects.update({k: v for k, v in fresh.items() if k[0] in {"pods", "replicaset"}})
        objects["deployment", "brood-go"]["status"] = fresh["deployment", "brood-go"]["status"]
        objects["service", "brood-go"]["metadata"]["uid"] = "replacement"
        with self.assertRaisesRegex(RuntimeError, "identity"):
            helper.inspect(IMAGE)


# FEATURE: Incompatible schema releases are refused before apply (#220).
# SPEC: on the healthy existing path the new image verifies the fresh snapshot
# (its path and sha256) after the snapshot and before the server apply, without
# the scale-to-zero opt-in. An incompatible schema stops the deploy before apply
# with the failing table, object or version and no row data; the running
# Deployment is not written, edge and probe never run, the lease is dropped.
# IMPLEMENTATION: ExistingInstall.protected_snapshot / failure over FakeKubernetes.
# STATUS: DONE
class HealthySchemaGateTests(IsolatedTestCase):
    PRIVATE = "synthetic-private-row password=hunter2"

    def run_healthy(self, refusal=None, schema=(16, 16), after=None):
        deploy, rollout, kube = CrashLoopRecoveryTests.real_flow(self, healthy=True)
        # Без BROOD_DEPLOY_SCALE_FENCE: gate не зависит от opt-in recovery.
        deploy.get = {**VALUES, "BROOD_DEPLOY_MODE": "existing", "BROOD_ADMIN_PASSWORD": ""}.get
        kube.schema, kube.after_version = schema, after
        if refusal is not None:
            kube.refuse["verify"] = refusal
        helpers = []
        kube.on_create = lambda k: helpers.append(copy.deepcopy(k.created["pod"]["spec"]["containers"][0]))
        before = copy.deepcopy(kube.objects["deployment", "brood-go"])
        with patch.object(public, "GoDeploy") as go:
            go.return_value.run.side_effect = rollout
            try:
                result = deploy.run()
            except RuntimeError as error:
                result = error
        return result, deploy, kube, before, helpers

    def test_compatible_snapshot_is_verified_by_the_new_image_before_apply(self):
        result, _, kube, _, helpers = self.run_healthy()
        self.assertNotIn("recovered", result)
        kinds = [e for e in kube.events if e[0] in {"logs", "apply"}]
        self.assertEqual(kinds, [("logs", "snapshot"), ("logs", "verify"), ("apply",), ("logs", "inspect")])
        snapshot, verify = helpers[0], helpers[1]
        self.assertEqual(verify["image"], IMAGE)
        self.assertEqual(verify["command"], ["/brood", "database", "verify"])
        self.assertEqual(verify["args"], ["--path", snapshot["args"][3], "--sha256", "a" * 64])
        self.assertEqual(snapshot["args"][3], result["snapshot"])

    def test_incompatible_schema_refuses_before_apply_and_keeps_the_running_server(self):
        causes = {
            "table": "incompatible database schema: table swarms differs from current schema",
            "version": "incompatible database schema: found 15, want 16",
            "trigger": "incompatible database schema: missing trigger swarm_graph_immutable",
            "index": "incompatible database schema: index swarms_live_name differs from current schema",
            # Миграции (#274): старый образ на новой базе, версия вне цепочки, упавший шаг.
            "newer": "incompatible database schema: found 27, want 26; the database is newer than this release",
            "unknown": "incompatible database schema: found 15, want 27; no migration from version 15",
            "step": "incompatible database schema: migration from version 26 to 27 failed",
        }
        for name, cause in causes.items():
            with self.subTest(name):
                log = f"{self.PRIVATE}\nbrood database: {cause}: {self.PRIVATE}\n"
                error, deploy, kube, before, _ = self.run_healthy(log)
                self.assertIsInstance(error, RuntimeError)
                message = str(error)
                self.assertIn(cause, message)
                self.assertIn("before apply", message)
                self.assertIn("ships with a migration from the deployed version", message)
                self.assertNotIn("recreat", message)
                self.assertNotIn("hunter2", message)
                self.assertNotIn("private-row", message)
                # Production остаётся на прежней версии: Deployment не записан, apply не было.
                self.assertNotIn(("apply",), kube.events)
                self.assertEqual([e for e in kube.events if e[0] in {"patch", "annotate", "probe"}], [])
                self.assertEqual(kube.objects["deployment", "brood-go"], before)
                deploy.apply_edge.assert_not_called()
                deploy.probe.assert_not_called()
                # Оба helper (snapshot и verify) удалены по UID, Lease снята: повтор не заблокирован.
                self.assertEqual(len(kube.deleted), 4)
                self.assertIsNone(kube.objects.get(("lease", "brood-go-deploy")))

    # FEATURE: Schema gate passes a release that migrates the database (#274).
    # SPEC: the snapshot is version N, the new image verifies it by migrating its
    # bytes in memory and reports N and the target M; the rollout applies, the
    # post-rollout inspect must read M, and the result names both versions.
    # STATUS: DONE
    def test_older_database_the_release_migrates_is_rolled_out(self):
        result, deploy, kube, _, _ = self.run_healthy(schema=(26, 27))
        self.assertNotIsInstance(result, Exception)
        self.assertEqual(result["schema"], {"from": 26, "to": 27})
        kinds = [e for e in kube.events if e[0] in {"logs", "apply"}]
        self.assertEqual(kinds, [("logs", "snapshot"), ("logs", "verify"), ("apply",), ("logs", "inspect")])
        self.assertIsNone(kube.objects.get(("lease", "brood-go-deploy")))

    def test_rollout_whose_database_is_not_at_the_verified_target_needs_operator_review(self):
        error, deploy, kube, _, _ = self.run_healthy(schema=(26, 27), after=26)
        self.assertIsInstance(error, RuntimeError)
        self.assertIn("schema version after rollout", str(error))
        self.assertIn(("apply",), kube.events)
        # После apply исход неоднозначен: Lease остаётся до решения оператора.
        self.assertIsNotNone(kube.objects.get(("lease", "brood-go-deploy")))

    def test_verify_failure_without_schema_cause_still_refuses_before_apply(self):
        error, deploy, kube, before, _ = self.run_healthy("panic: " + self.PRIVATE + "\n")
        self.assertIsInstance(error, RuntimeError)
        self.assertIn("verify failed", str(error))
        self.assertNotIn("hunter2", str(error))
        self.assertNotIn(("apply",), kube.events)
        self.assertEqual(kube.objects["deployment", "brood-go"], before)


# FEATURE: Exclusive-writer protocol before the recovery snapshot (#220).
# SPEC: snapshot of a live PVC proves nothing while a CrashLoop pod restarts or
# another pod mounts the claim. The one confirmed Deployment is scaled to zero
# with uid/resourceVersion preconditions, the concrete pod UID must disappear,
# any other pod mounting the PVC is a refusal, identities are rechecked at every
# stage, and no server apply happens without a verified snapshot.
# IMPLEMENTATION: ExistingInstall.quiesce/fence/release over kubectl patch.
class ExclusiveWriterTests(IsolatedTestCase):
    def setUp(self):
        self.time = 0.0

    def clock(self):
        return self.time

    def sleep(self, seconds):
        self.time += seconds

    def flow(self, **options):
        deploy, rollout, kube = CrashLoopRecoveryTests.real_flow(self, **options)
        return deploy, rollout, kube

    def run_flow(self, kube_setup=None):
        deploy, rollout, kube = self.flow()
        if kube_setup:
            kube_setup(kube)
        with patch.object(public, "GoDeploy") as go:
            go.return_value.run.side_effect = rollout
            try:
                return deploy.run(), kube
            except RuntimeError as error:
                return error, kube

    def assertRefusedBeforeAnyHelper(self, result, kube, pattern):
        self.assertIsInstance(result, RuntimeError)
        self.assertRegex(str(result), pattern)
        self.assertNotIn(("create", "Pod"), kube.events)
        self.assertNotIn(("apply",), kube.events)

    def replicas(self, kube):
        return kube.objects["deployment", "brood-go"]["spec"]["replicas"]

    def test_foreign_pods_mounting_the_claim_are_refused_before_scale_or_helper(self):
        cases = {"running": {}, "pending": {"phase": "Pending"}, "unlabeled terminating": {"phase": "Running"}}
        for name, options in cases.items():
            with self.subTest(name=name):
                def setup(kube, options=options):
                    pod = kube.foreign_pod(**options)
                    if name == "unlabeled terminating":
                        pod["metadata"]["deletionTimestamp"] = "2026-11-09T00:00:00Z"
                result, kube = self.run_flow(setup)
                self.assertRefusedBeforeAnyHelper(result, kube, "unexpected pod")
                self.assertNotIn(("patch", 0), kube.events)
                self.assertEqual(self.replicas(kube), 1)

    def test_pod_mounting_the_claim_right_after_scale_is_refused_before_helper(self):
        result, kube = self.run_flow(lambda k: setattr(k, "after_scale", lambda c: c.foreign_pod(phase="Pending")))
        self.assertRefusedBeforeAnyHelper(result, kube, "unexpected pod")
        self.assertEqual(self.replicas(kube), 1)

    def test_pods_of_other_claims_and_finished_pods_do_not_block(self):
        def setup(kube):
            kube.foreign_pod(claim="unrelated", name="other", uid="other-uid")
            kube.foreign_pod(phase="Succeeded", name="done", uid="done-uid")
            kube.foreign_pod(phase="Failed", name="dead", uid="dead-uid")
        result, kube = self.run_flow(setup)
        self.assertEqual(result["recovered"], "crashloop")

    def test_writer_is_gone_before_the_first_helper_and_apply_is_last(self):
        result, kube = self.run_flow()
        self.assertEqual(result["recovered"], "crashloop")
        names = [e for e in kube.events if e[0] in {"patch", "create", "apply"}]
        self.assertEqual(names[0], ("patch", 0))
        self.assertLess(names.index(("patch", 0)), names.index(("create", "Pod")))
        self.assertLess(names.index(("create", "Pod")), names.index(("apply",)))
        self.assertEqual(len([e for e in names if e == ("patch", 0)]), 1)

    def test_patch_carries_uid_and_resource_version_preconditions(self):
        deploy, rollout, kube = self.flow()
        seen = []
        original = kube.patch

        def spy(args):
            if "--dry-run=server" not in args:
                seen.append(json.loads(args[args.index("-p") + 1]))
            return original(args)
        kube.patch = spy
        with patch.object(public, "GoDeploy") as go:
            go.return_value.run.side_effect = rollout
            deploy.run()
        self.assertEqual(seen[0]["metadata"]["uid"], "brood-go-uid")
        self.assertEqual(seen[0]["metadata"]["resourceVersion"], "100")
        self.assertNotIn("spec", seen[0])  # намерение: replicas ещё не тронуты
        record = json.loads(seen[0]["metadata"]["annotations"]["brood.dev/quiesce"])
        self.assertEqual((record["uid"], record["phase"], record["pod"]), ("brood-go-uid", "intent", "pod-uid"))
        self.assertNotIn("gen", record)  # generation не доказательство: его двигает и сама аннотация
        scale = seen[1]
        self.assertEqual(scale["spec"], {"replicas": 0})
        self.assertEqual(scale["metadata"]["uid"], "brood-go-uid")
        self.assertEqual(scale["metadata"]["resourceVersion"], "101")  # RV подтверждённого намерения
        # Тот же patch переводит запись в phase=scaled: replicas=0 и scaled живут только вместе.
        self.assertEqual(json.loads(scale["metadata"]["annotations"]["brood.dev/quiesce"]), {**record, "phase": "scaled"})

    def test_resource_version_race_is_retried_with_fresh_proof_then_succeeds(self):
        def setup(kube):
            def race(cluster):
                kube.on_patch = None
                cluster.bump()  # status-обновление контроллера между чтением и patch
            kube.on_patch = race
        result, kube = self.run_flow(setup)
        self.assertEqual(result["recovered"], "crashloop")
        self.assertEqual(kube.events.count(("annotate",)) >= 2, True)
        self.assertEqual(kube.events.count(("patch", 0)), 1)

    def test_persistent_conflict_refuses_without_helper_and_leaves_replicas(self):
        def setup(kube):
            kube.on_patch = lambda cluster: cluster.bump()
        result, kube = self.run_flow(setup)
        self.assertRefusedBeforeAnyHelper(result, kube, "could not be confirmed")
        self.assertEqual(self.replicas(kube), 1)

    def test_deployment_uid_swap_between_proof_and_patch_is_refused(self):
        def setup(kube):
            def swap(cluster):
                cluster.objects["deployment", "brood-go"]["metadata"]["uid"] = "replacement"
            kube.on_patch = swap
        result, kube = self.run_flow(setup)
        # Подмена во время пробы: probe не получает ожидаемый конфликт RV и отказывает закрыто.
        self.assertRefusedBeforeAnyHelper(result, kube, "identity|did not prove enforcement")
        self.assertEqual(self.replicas(kube), 1)

    def test_scale_failure_stops_before_helper_and_apply(self):
        result, kube = self.run_flow(lambda k: setattr(k, "fail_scale", True))
        self.assertRefusedBeforeAnyHelper(result, kube, "could not be confirmed")
        self.assertEqual(self.replicas(kube), 1)

    def test_lost_scale_response_is_resolved_by_rereading_the_recorded_state(self):
        # Ответ потерян, но запись и scale применились: повторное чтение подтверждает
        # состояние, выкладка идёт дальше и ровно один раз пишет replicas=0.
        result, kube = self.run_flow(lambda k: setattr(k, "fail_scale_after", 1))
        self.assertEqual(result["recovered"], "crashloop")
        self.assertEqual(kube.events.count(("patch", 0)), 1)
        self.assertNotIn("brood.dev/quiesce", kube.objects["deployment", "brood-go"]["metadata"].get("annotations", {}))

    def test_lost_intent_response_is_confirmed_by_reading_not_by_the_reply(self):
        result, kube = self.run_flow(lambda k: setattr(k, "lose_intent", 1))
        self.assertEqual(result["recovered"], "crashloop")
        self.assertEqual(kube.events.count(("annotate",)) >= 1, True)
        self.assertEqual(kube.events.index(("annotate",)), kube.events.index(("patch", 0)) - 1)

    def test_intent_is_durable_before_the_first_replicas_change(self):
        deploy, rollout, kube = self.flow()
        seen = []
        original = kube.patch

        def spy(args):
            body = json.loads(args[args.index("-p") + 1])
            if "--dry-run=server" not in args:
                notes = kube.objects["deployment", "brood-go"]["metadata"].get("annotations", {})
                seen.append((body.get("spec", {}).get("replicas"), "brood.dev/quiesce" in notes))
            return original(args)
        kube.patch = spy
        with patch.object(public, "GoDeploy") as go:
            go.return_value.run.side_effect = rollout
            deploy.run()
        # patch replicas=0 стартует, когда запись уже лежит в объекте.
        self.assertIn((0, True), seen)
        self.assertNotIn((0, False), seen)

    def test_intent_not_applied_means_no_scale_and_no_record(self):
        result, kube = self.run_flow(lambda k: setattr(k, "drop_intent", 99))
        self.assertIsInstance(result, RuntimeError)
        self.assertNotIn(("patch", 0), kube.events)
        self.assertEqual(self.replicas(kube), 1)
        self.assertNotIn(("create", "Pod"), kube.events)

    def test_pod_that_never_disappears_times_out_and_replicas_are_restored(self):
        helper, objects = self.helper_with_clock()
        self.cluster.linger = True
        helper.state = helper.check()
        with self.assertRaisesRegex(RuntimeError, "did not terminate"):
            helper.snapshot(IMAGE)
        self.assertNotIn(("create", "Pod"), self.cluster.events)
        self.assertEqual(objects["deployment", "brood-go"]["spec"]["replicas"], 1)
        self.assertGreaterEqual(self.time, 120)

    def helper_with_clock(self):
        return CrashLoopRecoveryTests.helper(self, clock=self.clock, sleep=self.sleep)

    def test_helper_failure_midway_restores_replicas_and_never_applies(self):
        for operation in ("snapshot", "verify"):
            with self.subTest(operation=operation):
                result, kube = self.run_flow(lambda k, op=operation: setattr(k, "fail_op", op))
                self.assertIsInstance(result, RuntimeError)
                self.assertNotIn(("apply",), kube.events)
                self.assertEqual(self.replicas(kube), 1)
                self.assertEqual(kube.events.count(("patch", 1)), 1)
                self.assertEqual(len(kube.deleted), 2 if operation == "snapshot" else 4)

    def test_foreign_pod_appearing_while_helper_runs_refuses_and_restores(self):
        result, kube = self.run_flow(lambda k: setattr(k, "on_create", lambda c: c.foreign_pod(phase="Pending")))
        self.assertIsInstance(result, RuntimeError)
        self.assertRegex(str(result), "unexpected pod")
        self.assertNotIn(("apply",), kube.events)
        self.assertNotIn(("logs", "verify"), kube.events)
        self.assertEqual(self.replicas(kube), 1)

    def test_pvc_identity_swap_after_scale_refuses_and_still_restores(self):
        def setup(kube):
            def swap(cluster):
                cluster.objects["pvc", "brood-go-data"]["metadata"]["uid"] = "replacement"
            kube.on_create = swap
        result, kube = self.run_flow(setup)
        self.assertRegex(str(result), "identity")
        self.assertNotIn(("apply",), kube.events)

    def test_third_party_spec_change_after_scale_is_never_overwritten(self):
        def setup(kube):
            def edit(cluster):
                deploy = cluster.objects["deployment", "brood-go"]
                deploy["metadata"]["generation"] += 1
                deploy["spec"]["template"]["spec"]["containers"][0]["image"] = "localhost:5002/brood-go@sha256:" + "d" * 64
                cluster.bump()
            kube.on_create = edit
        result, kube = self.run_flow(setup)
        self.assertRegex(str(result), "operator|identity|replicas=0")
        self.assertNotIn(("patch", 1), kube.events)
        self.assertNotIn(("apply",), kube.events)
        self.assertEqual(self.replicas(kube), 0)

    def test_failed_run_cleans_up_and_a_repeat_run_succeeds(self):
        deploy, rollout, kube = self.flow()
        kube.fail_op = "verify"
        with patch.object(public, "GoDeploy") as go:
            go.return_value.run.side_effect = rollout
            with self.assertRaises(RuntimeError):
                deploy.run()
            self.assertEqual(self.replicas(kube), 1)
            self.assertEqual(kube.brood_pods()[0]["metadata"]["uid"], "pod-uid+")  # контроллер поднял pod заново
            kube.fail_op, kube.created = None, {}
            result = deploy.run()
        self.assertEqual(result["recovered"], "crashloop")
        self.assertEqual(kube.events.count(("apply",)), 1)

    def test_unparseable_pod_listing_refuses(self):
        helper, _ = self.helper_with_clock()
        helper.state = helper.check()
        original = self.cluster.output
        helper.deployment.cluster.shell.output.side_effect = (
            lambda *a: (0, "not json", "") if "get" in a and "pods" in a and "-l=app=brood-go" not in a else original(*a))
        with self.assertRaisesRegex(RuntimeError, "cannot list pods"):
            helper.snapshot(IMAGE)
        self.assertNotIn(("patch", 0), self.cluster.events)

    def test_healthy_runtime_is_never_scaled_or_fenced(self):
        deploy, rollout, kube = CrashLoopRecoveryTests.real_flow(self, healthy=True)
        kube.foreign_pod()  # у штатного пути прежняя семантика: он не знает про fence
        with patch.object(public, "GoDeploy") as go:
            go.return_value.run.side_effect = rollout
            deploy.run()
        self.assertEqual([e for e in kube.events if e[0] == "patch"], [])


# FEATURE: Durable, re-enterable scale-to-zero protocol (#220 blocker).
# SPEC: the intent (uid, generation, spec digest, pod UID, token) is written into the
# Deployment by a CAS patch BEFORE replicas change and read back, never trusted from a
# reply; an abruptly stopped run is settled by the next one purely from that record
# (uid + spec digest + generation, never by name); a stale or late patch loses on
# resourceVersion once any later write happened; anything unprovable fails closed
# with an exact operator procedure and without touching a third party's spec.
# IMPLEMENTATION: ExistingInstall.quiesce / fence_late / settle / recover.
class DurableQuiesceTests(IsolatedTestCase):
    NOTE = "brood.dev/quiesce"

    def setUp(self):
        self.suite = ExclusiveWriterTests("test_foreign_pods_mounting_the_claim_are_refused_before_scale_or_helper")
        self.suite.setUp()
        self.deploy, self.rollout, self.kube = CrashLoopRecoveryTests.real_flow(self.suite)
        self.dep = self.kube.objects["deployment", "brood-go"]

    def process(self, now=1000.0, **options):
        """Новый процесс deploy: свежий ExistingInstall над тем же кластером и своими часами."""
        from brood.dev.deploy.existing_install import ExistingInstall
        timing = FakeTime()
        self.last_time = timing
        install = ExistingInstall(self.deploy, wall=lambda: now, **{**timing.options(), **options})
        return self.track(install)

    def record(self):
        raw = self.dep["metadata"].get("annotations", {}).get(self.NOTE)
        return json.loads(raw) if raw else None

    def killed_after_scale(self):
        """Процесс дошёл до подтверждённого replicas=0 и исчез (SIGKILL): release не вызывался."""
        first = self.process()
        first.state = first.check()
        first.quiesce(first.state)
        self.assertEqual(self.dep["spec"]["replicas"], 0)
        first.unlock()  # процесс исчез: ядро снимает flock, Lease остаётся
        return first

    def test_sigkill_after_scale_is_settled_by_the_next_process_from_the_record(self):
        self.killed_after_scale()
        self.assertEqual(self.record()["uid"], "brood-go-uid")
        # Preflight --check ничего не пишет и называет причину.
        events = len(self.kube.events)
        with patch.object(public, "Infrastructure"), patch.object(public, "GoDeploy"):
            with self.assertRaisesRegex(RuntimeError, "lease/brood-go-deploy exists"):
                public.Deployment.check(self.deploy)  # Lease мёртвого процесса - отказ, не "recoverable"
        self.assertEqual(len(self.kube.events), events)
        self.operator_clears_lease()
        self.assertEqual(self.process(now=1000.0 + 300).check(recoverable=True)["mode"], "recoverable")
        self.assertEqual(len(self.kube.events), events)
        second = self.process(now=1000.0 + 300)
        with self.assertRaisesRegex(RuntimeError, "replicas=1 was restored.*rerun"):
            second.recover()
        self.assertEqual(self.dep["spec"]["replicas"], 1)
        self.assertIsNone(self.record())
        self.assertEqual(self.dep["spec"]["template"]["spec"]["containers"][0]["image"], IMAGE)

    def test_full_run_check_settles_before_the_install_preflight(self):
        self.killed_after_scale()
        self.operator_clears_lease()
        existing = self.process(now=5000.0)
        with patch.object(public, "Infrastructure"), patch.object(public, "GoDeploy"):
            with self.assertRaisesRegex(RuntimeError, "replicas=1 was restored"):
                public.Deployment.check(self.deploy, existing)
        self.assertEqual(self.dep["spec"]["replicas"], 1)
        self.assertIsNone(self.record())

    def test_restored_deployment_recovers_on_the_next_run_after_the_pod_crashes_again(self):
        self.killed_after_scale()
        self.operator_clears_lease()
        second = self.process(now=5000.0)
        with self.assertRaises(RuntimeError):
            second.recover()
        second.release()
        result = None
        with patch.object(public, "GoDeploy") as go:
            go.return_value.run.side_effect = self.rollout
            result = self.deploy.run()
        self.assertEqual(result["recovered"], "crashloop")

    def test_young_record_is_never_touched(self):
        self.killed_after_scale()
        events = len(self.kube.events)
        with self.assertRaisesRegex(RuntimeError, "only .*s old.*nothing was changed"):
            self.process(now=1000.0 + 10).recover()
        self.assertEqual(len(self.kube.events), events)
        self.assertEqual(self.dep["spec"]["replicas"], 0)

    def test_sigkill_between_intent_and_scale_aborts_the_intent_and_kills_the_in_flight_scale(self):
        first = self.process()
        first.state = first.check()
        in_flight = []

        def killed(cluster):
            raise SystemExit("SIGKILL")  # процесс исчез, а запрос scale(0) уже в пути
        original = self.kube.patch

        def intercept(args):
            body = json.loads(args[args.index("-p") + 1])
            if body.get("spec", {}).get("replicas") == 0:
                in_flight.append(body)
                raise SystemExit("SIGKILL")
            return original(args)
        self.kube.patch = intercept
        with self.assertRaises(SystemExit):
            first.quiesce(first.state)
        self.operator_clears_lease(first)
        self.kube.patch = original
        self.assertEqual(self.dep["spec"]["replicas"], 1)
        self.assertIsNotNone(self.record())  # намерение пережило процесс
        second = self.process(now=1000.0 + 300)
        self.assertEqual(second.recover(), "cleared")
        second.release()
        self.assertIsNone(self.record())
        # Запоздавший запрос доходит до API: resourceVersion уже другой.
        self.assertFalse(self.kube.apply_patch(in_flight[0]))
        self.assertEqual(self.dep["spec"]["replicas"], 1)

    def test_late_scale_after_unknown_outcome_loses_to_the_fence_write(self):
        first = self.process()
        first.state = first.check()
        self.kube.hold_scale = True
        with self.assertRaises(RuntimeError):
            first.quiesce(first.state)
        held = len(self.kube.late)
        self.assertGreaterEqual(held, 1)
        self.assertGreaterEqual(self.record()["fence"], 1)
        self.assertEqual(self.dep["spec"]["replicas"], 1)
        # Запоздавшие запросы доходят до API после fence-записи: все дают конфликт.
        self.assertEqual(self.kube.deliver_late(), [False] * held)
        self.assertEqual(self.dep["spec"]["replicas"], 1)
        first.release()
        self.assertIsNone(self.record())
        self.assertEqual(self.dep["spec"]["replicas"], 1)
        self.assertEqual(self.kube.brood_pods()[0]["metadata"]["name"], "server")

    def test_late_scale_is_dead_even_after_release_and_a_second_process(self):
        first = self.process()
        first.state = first.check()
        self.kube.hold_scale = True
        with self.assertRaises(RuntimeError):
            first.quiesce(first.state)
        first.release()
        second = self.process(now=9999.0)
        self.assertIsNone(second.recover())
        delivered = self.kube.deliver_late()
        self.assertTrue(delivered)
        self.assertNotIn(True, delivered)
        self.assertEqual(self.dep["spec"]["replicas"], 1)

    def test_stale_resource_version_or_uid_is_rejected_by_the_cas_model(self):
        body = {"metadata": {"uid": "brood-go-uid", "resourceVersion": "1"}, "spec": {"replicas": 0}}
        self.assertFalse(self.kube.apply_patch(body))
        body = {"metadata": {"uid": "someone-else", "resourceVersion": "100"}, "spec": {"replicas": 0}}
        self.assertFalse(self.kube.apply_patch(body))
        self.assertEqual(self.dep["spec"]["replicas"], 1)

    def test_reply_with_another_uid_is_not_accepted(self):
        install = self.process()
        current = self.kube.get_object("deployment", "brood-go")
        original = self.kube.output

        def wrong(*args):
            code, out, err = original(*args)
            if "patch" in args and code == 0:
                out = out.replace('"uid": "brood-go-uid"', '"uid": "other"', 1)
            return code, out, err
        self.deploy.cluster.shell.output.side_effect = wrong
        with self.assertRaisesRegex(RuntimeError, "does not match its preconditions"):
            install.write(current, "{}")

    def test_record_with_wrong_uid_or_garbage_fails_closed_without_writing(self):
        for raw in ("not json", json.dumps({"v": 1}), "[]"):
            with self.subTest(raw=raw):
                self.dep["metadata"].setdefault("annotations", {})[self.NOTE] = raw
                events = len(self.kube.events)
                with self.assertRaisesRegex(RuntimeError, "cannot be verified.*docs/DEPLOY.md"):
                    self.recovered_by_next(9999)
                self.assertEqual(len(self.kube.events), events)
        # Корректная запись, но чужой Deployment UID.
        self.dep["metadata"]["annotations"].pop(self.NOTE)
        self.killed_after_scale()
        note = self.record()
        note["uid"] = "someone-else"
        self.dep["metadata"]["annotations"][self.NOTE] = json.dumps(note)
        with self.assertRaisesRegex(RuntimeError, "cannot be verified"):
            self.recovered_by_next(9999)
        self.assertEqual(self.dep["spec"]["replicas"], 0)

    def test_third_party_spec_edit_after_a_killed_run_is_never_overwritten(self):
        self.killed_after_scale()
        self.dep["spec"]["template"]["spec"]["containers"][0]["image"] = "localhost:5002/brood-go@sha256:" + "e" * 64
        self.dep["metadata"]["generation"] += 1
        events = len(self.kube.events)
        self.operator_clears_lease()
        events = len(self.kube.events)
        with self.assertRaisesRegex(RuntimeError, "not the state this deploy recorded.*docs/DEPLOY.md"):
            self.recovered_by_next(9999)
        self.assertEqual(len(self.kube.events), events)
        self.assertEqual(self.dep["spec"]["replicas"], 0)
        self.assertIsNotNone(self.record())

    def test_record_removed_while_still_at_zero_is_refused_not_guessed(self):
        first = self.killed_after_scale()
        self.dep["metadata"]["annotations"].pop(self.NOTE)
        with self.assertRaisesRegex(RuntimeError, "may be left at replicas=0"):
            first.release()
        self.assertEqual(self.dep["spec"]["replicas"], 0)

    def test_release_refuses_a_record_of_another_deploy(self):
        first = self.killed_after_scale()
        note = self.record()
        note["id"] = "f" * 32
        self.dep["metadata"]["annotations"][self.NOTE] = json.dumps(note)
        with self.assertRaisesRegex(RuntimeError, "may be left at replicas=0"):
            first.release()
        self.assertEqual(self.dep["spec"]["replicas"], 0)

    def test_failure_after_partial_apply_clears_only_the_record_and_keeps_the_new_spec(self):
        new_image = "localhost:5002/brood-go@sha256:" + "c" * 64

        def partial(*, before_apply):
            before_apply(IMAGE)
            self.dep["spec"]["replicas"] = 1
            self.dep["spec"]["template"]["spec"]["containers"][0]["image"] = new_image
            self.dep["metadata"]["generation"] += 1
            self.kube.bump()
            raise RuntimeError("apply failed midway")
        with patch.object(public, "GoDeploy") as go:
            go.return_value.run.side_effect = partial
            with self.assertRaisesRegex(RuntimeError, "apply failed midway"):
                self.deploy.run()
        self.assertIsNone(self.record())
        self.assertEqual(self.dep["spec"]["replicas"], 1)
        self.assertEqual(self.dep["spec"]["template"]["spec"]["containers"][0]["image"], new_image)
        self.assertNotIn(("patch", 1), self.kube.events)

    def test_failure_before_apply_restores_replicas_with_cas_and_removes_the_record(self):
        def refused(*, before_apply):
            before_apply(IMAGE)
            raise RuntimeError("apply refused")
        with patch.object(public, "GoDeploy") as go:
            go.return_value.run.side_effect = refused
            with self.assertRaisesRegex(RuntimeError, "apply refused"):
                self.deploy.run()
        self.assertEqual(self.dep["spec"]["replicas"], 1)
        self.assertIsNone(self.record())
        self.assertEqual(self.kube.events.count(("patch", 1)), 1)

    def test_snapshot_refusal_leaves_replicas_restored_and_no_record(self):
        self.kube.fail_op = "snapshot"
        with patch.object(public, "GoDeploy") as go:
            go.return_value.run.side_effect = self.rollout
            with self.assertRaises(RuntimeError):
                self.deploy.run()
        self.assertEqual(self.dep["spec"]["replicas"], 1)
        self.assertIsNone(self.record())
        self.assertNotIn(("apply",), self.kube.events)

    def test_pod_listing_failure_before_any_write_changes_nothing(self):
        self.kube.fail_get_pods = True
        with patch.object(public, "GoDeploy") as go:
            go.return_value.run.side_effect = self.rollout
            with self.assertRaisesRegex(RuntimeError, "owned database helper failed|cannot list pods"):
                self.deploy.run()
        self.assertEqual([e for e in self.kube.events if e[0] in {"patch", "annotate"}], [])
        self.assertIsNone(self.record())

    def test_pod_listing_failure_after_scale_refuses_and_restores(self):
        self.kube.after_scale = lambda cluster: setattr(cluster, "fail_get_pods", True)
        with patch.object(public, "GoDeploy") as go:
            go.return_value.run.side_effect = self.rollout
            with self.assertRaises(RuntimeError):
                self.deploy.run()
        self.kube.fail_get_pods = False
        self.assertEqual(self.dep["spec"]["replicas"], 1)
        self.assertIsNone(self.record())
        self.assertNotIn(("create", "Pod"), self.kube.events)
        self.assertNotIn(("apply",), self.kube.events)

    def test_foreign_pod_is_refused_before_the_intent_is_written(self):
        self.kube.foreign_pod()
        with patch.object(public, "GoDeploy") as go:
            go.return_value.run.side_effect = self.rollout
            with self.assertRaisesRegex(RuntimeError, "unexpected pod"):
                self.deploy.run()
        self.assertEqual([e for e in self.kube.events if e[0] in {"patch", "annotate"}], [])

    def test_api_that_does_not_enforce_preconditions_blocks_scaling(self):
        self.kube.ignore_preconditions = True
        with patch.object(public, "GoDeploy") as go:
            go.return_value.run.side_effect = self.rollout
            with self.assertRaisesRegex(RuntimeError, "did not prove enforcement"):
                self.deploy.run()
        self.assertEqual([e for e in self.kube.events if e[0] in {"patch", "annotate"}], [])

    def test_probe_accepts_only_the_exact_kubectl_text_for_each_case(self):
        from brood.dev.deploy.existing_install import ExistingInstall
        # Реальный kubectl 1.36: без "Error from server", с предвычисленным чужим UID.
        exact = INVALID_UID.format(want=ExistingInstall.ALIEN_UID)
        self.assertEqual(exact, 'The Deployment "brood-go" is invalid: metadata.uid: Invalid value: '
                                '"00000000-0000-0000-0000-000000000000": field is immutable')
        self.assert_probe(CONFLICT_RV, exact, ok=True)
        self.assert_probe(CONFLICT_RV, exact + "\n", ok=True)

    def test_probe_rejects_foreign_errors_that_merely_mention_uid_or_modified(self):
        from brood.dev.deploy.existing_install import ExistingInstall
        alien = ExistingInstall.ALIEN_UID
        uid_ok = INVALID_UID.format(want=alien)
        foreign = (
            # прежние формы: префикс и precondition-конфликт больше не принимаются
            'Error from server (Invalid): ' + uid_ok,
            CONFLICT_UID.format(want=alien, have="u"),
            # чужой UID и чужое имя
            INVALID_UID.format(want="a-b-1"),
            INVALID_UID.format(want=""),
            INVALID_UID.format(want=alien[:-1] + "1"),
            INVALID_UID.format(want=alien).replace("brood-go", "edge"),
            uid_ok.replace("The Deployment", "Deployment.apps"),
            # дописки, перенос, дубль и webhook
            uid_ok + "; webhook says UID",
            uid_ok + "\n" + uid_ok,
            uid_ok + "\nanything",
            uid_ok + "\n\n",
            " " + uid_ok,
            'admission webhook "x" denied the request: ' + uid_ok,
            'Error from server (Forbidden): ' + uid_ok,
            'Error from server (InternalError): Internal error occurred: admission webhook "x" denied '
            'the request: UID is modified',
            'Error from server (Forbidden): admission webhook "x" denied the request: UID modified',
            'The Deployment "brood-go" is invalid: spec.replicas: Invalid value: -1',
            'Error from server (Timeout): request did not complete within requested timeout - modified UID',
            'Unable to connect to the server: net/http: request canceled (Client.Timeout exceeded) UID modified',
            "",
        )
        for err in foreign:
            with self.subTest(err=err):
                self.assert_probe(CONFLICT_RV, err, ok=False)
        stale = (
            'Error from server (Conflict): admission webhook "x" denied the request: the object has been modified',
            'Error from server (Invalid): ' + CONFLICT_RV.split("): ", 1)[1],
            'Error from server (Forbidden): ' + CONFLICT_RV.split("): ", 1)[1],
            'Error from server (Forbidden): denied\n' + CONFLICT_RV,
            CONFLICT_RV + "\nanything",
            CONFLICT_RV + " UID",
            uid_ok,
            "",
        )
        for err in stale:
            with self.subTest(stale=err):
                self.assert_probe(err, uid_ok, ok=False)

    def test_probe_requires_the_matching_conflict_per_case(self):
        from brood.dev.deploy.existing_install import ExistingInstall
        self.assert_probe(INVALID_UID.format(want=ExistingInstall.ALIEN_UID), CONFLICT_RV, ok=False)

    def assert_probe(self, rv_err, uid_err, ok):
        from brood.dev.deploy.existing_install import ExistingInstall
        errors = iter((rv_err, uid_err))
        shell = Mock()
        shell.output.side_effect = lambda *a: (1, "", next(errors))
        install = ExistingInstall.__new__(ExistingInstall)
        install.deployment = Mock(cluster=Mock(shell=shell))
        current = {"metadata": {"uid": "u", "resourceVersion": "5"}}
        if ok:
            install.probe_preconditions(current)
            self.assertEqual(shell.output.call_count, 2)
        else:
            with self.assertRaisesRegex(RuntimeError, "did not prove enforcement"):
                install.probe_preconditions(current)

    def test_recovery_scale_is_blocked_unless_the_operator_opts_in(self):
        from brood.dev.deploy.existing_install import ExistingInstall
        blocked = ExistingInstall(public.Deployment(self.deploy.cluster, get={**VALUES, "BROOD_DEPLOY_MODE": "existing"}.get),
                                  fenced=None)
        blocked.deployment.get_object = self.deploy.get_object
        with self.assertRaisesRegex(RuntimeError, "blocked.*not verified on a live cluster"):
            blocked.check()
        self.assertEqual(self.kube.events, [])
        # Штатный здоровый runtime не затронут блокировкой.
        healthy, _ = ExistingInstallTests.helper(ExistingInstallTests("test_mode_is_explicit_and_existing_does_not_need_admin_password"))
        self.assertIsNone(healthy.check().get("mode"))


# FEATURE: Scale-to-zero protocol is correct under the real apiserver generation rules (#220).
# SPEC: apiserver 1.36 raises Deployment generation on any change of spec OR
# metadata.annotations, including our own record writes; it never raises it (nor
# resourceVersion) for a write that changes nothing. So no step predicts a generation: each
# step is confirmed by a read, identity is the uid plus a digest of the whole spec, and the
# record phase (intent -> scaled) travels in the same CAS as replicas=0.
# IMPLEMENTATION: ExistingInstall.classify / quiesce / fence_late / settle.
class GenerationModelTests(IsolatedTestCase):
    NOTE = "brood.dev/quiesce"

    def setUp(self):
        self.suite = ExclusiveWriterTests("test_foreign_pods_mounting_the_claim_are_refused_before_scale_or_helper")
        self.suite.setUp()
        self.deploy, self.rollout, self.kube = CrashLoopRecoveryTests.real_flow(self.suite)
        self.dep = self.kube.objects["deployment", "brood-go"]

    def process(self, now=1000.0):
        from brood.dev.deploy.existing_install import ExistingInstall
        timing = FakeTime()
        self.last_time = timing
        return self.track(ExistingInstall(self.deploy, wall=lambda: now, **timing.options()))

    def record(self):
        raw = self.dep["metadata"].get("annotations", {}).get(self.NOTE)
        return json.loads(raw) if raw else None

    def body(self, **extra):
        meta = self.dep["metadata"]
        return {"metadata": {"uid": meta["uid"], "resourceVersion": meta["resourceVersion"], **extra}}

    def test_fake_follows_the_real_generation_rules(self):
        meta = self.dep["metadata"]
        self.assertEqual(meta["generation"], 1)
        self.assertTrue(self.kube.apply_patch(self.body(annotations={self.NOTE: "{}"})))
        self.assertEqual((meta["generation"], meta["resourceVersion"]), (2, "101"))  # annotation: gen+1
        scale = self.body(annotations={self.NOTE: "{}"})
        scale["spec"] = {"replicas": 0}
        self.assertTrue(self.kube.apply_patch(scale))
        self.assertEqual((meta["generation"], meta["resourceVersion"]), (3, "102"))
        # Снятие отсутствующей аннотации и повтор того же scale - no-op: ни generation, ни RV.
        self.assertTrue(self.kube.apply_patch(self.body(annotations={"absent": None})))
        self.assertTrue(self.kube.apply_patch({**scale, "metadata": self.body(annotations={self.NOTE: "{}"})["metadata"]}))
        self.assertEqual((meta["generation"], meta["resourceVersion"]), (3, "102"))
        self.assertTrue(self.kube.apply_patch(self.body(annotations={self.NOTE: None})))
        self.assertEqual(meta["generation"], 4)

    def test_every_step_survives_generation_moving_on_annotation_writes(self):
        first = self.process()
        first.state = first.check()
        first.quiesce(first.state)
        record = self.record()
        # intent (+1) и scale с phase=scaled (+1): generation 1 -> 3, и это не мешает ни fence, ни recover.
        self.assertEqual((self.dep["metadata"]["generation"], record["phase"], self.dep["spec"]["replicas"]), (3, "scaled", 0))
        first.fence()
        with self.assertRaisesRegex(RuntimeError, "replicas=1 was restored"):
            self.recovered_by_next(2000)
        self.assertEqual((self.dep["spec"]["replicas"], self.record()), (1, None))

    def test_unrelated_annotation_from_a_third_party_does_not_break_quiesce_or_restore(self):
        first = self.process()
        first.state = first.check()
        # Чужая аннотация между check и записью намерения: generation и RV другие, spec тот же.
        self.assertTrue(self.kube.apply_patch(self.body(annotations={"team.example/note": "x"})))
        first.quiesce(first.state)
        self.assertTrue(self.kube.apply_patch(self.body(annotations={"team.example/other": "y"})))
        # Аннотация после подтверждённого scale двигает generation: fence блокирует (#220).
        with self.assertRaisesRegex(RuntimeError, "generation moved"):
            first.fence()
        first.release()
        self.assertEqual((self.dep["spec"]["replicas"], self.record()), (1, None))
        self.assertEqual(self.dep["metadata"]["annotations"]["team.example/other"], "y")

    def test_spec_change_between_check_and_intent_is_refused_without_writes(self):
        first = self.process()
        first.state = first.check()
        self.dep["spec"]["template"]["spec"]["containers"][0]["env"].append({"name": "X", "value": "1"})
        self.dep["metadata"]["generation"] += 1
        self.kube.bump()
        events = len(self.kube.events)
        with self.assertRaisesRegex(RuntimeError, "identity changed"):
            first.quiesce(first.state)
        self.assertEqual(len(self.kube.events), events)
        self.assertIsNone(self.record())

    def test_spec_change_with_generation_untouched_is_still_caught_by_digest(self):
        # generation нельзя использовать как доказательство ни в одну сторону: проверяем digest.
        first = self.process()
        first.state = first.check()
        first.quiesce(first.state)
        self.dep["spec"]["template"]["spec"]["containers"][0]["image"] = "localhost:5002/brood-go@sha256:" + "9" * 64
        with self.assertRaisesRegex(RuntimeError, "identity changed"):
            first.fence()
        with self.assertRaisesRegex(RuntimeError, "may be left at replicas=0"):
            first.release()
        self.assertEqual(self.dep["spec"]["replicas"], 0)

    def test_replicas_zero_with_a_non_scaled_phase_is_foreign(self):
        first = self.process()
        first.state = first.check()
        first.quiesce(first.state)
        note = self.record()
        note["phase"] = "intent"
        self.dep["metadata"]["annotations"][self.NOTE] = json.dumps(note)
        events = len(self.kube.events)
        with self.assertRaisesRegex(RuntimeError, "not the state this deploy recorded"):
            self.recovered_by_next(9999)
        self.assertEqual((len(self.kube.events), self.dep["spec"]["replicas"]), (events, 0))

    def test_stale_record_after_apply_is_idle_and_only_the_record_is_cleared(self):
        first = self.process()
        first.state = first.check()
        first.quiesce(first.state)
        # Apply вернул replicas=1 с новым образом, процесс исчез, запись осталась.
        self.dep["spec"]["replicas"] = 1
        self.dep["spec"]["template"]["spec"]["containers"][0]["image"] = "localhost:5002/brood-go@sha256:" + "8" * 64
        self.dep["metadata"]["generation"] += 1
        self.kube.bump()
        self.assertEqual(self.recovered_by_next(9999), "cleared")
        self.assertEqual((self.dep["spec"]["replicas"], self.record()), (1, None))
        self.assertTrue(self.dep["spec"]["template"]["spec"]["containers"][0]["image"].endswith("8" * 64))
        self.assertNotIn(("patch", 1), self.kube.events)

    def test_lost_response_of_every_write_is_resolved_by_reading(self):
        for knob in ("lose_intent", "fail_scale_after", "lose_restore"):
            with self.subTest(knob=knob):
                self.setUp()
                setattr(self.kube, knob, 1)
                first = self.process()
                first.state = first.check()
                first.quiesce(first.state)
                self.assertEqual((self.record()["phase"], self.dep["spec"]["replicas"]), ("scaled", 0))
                self.assertEqual(self.kube.events.count(("patch", 0)), 1)
                first.release()  # lose_restore теряет ответ снятия записи: итог тот же
                self.assertEqual((self.dep["spec"]["replicas"], self.record()), (1, None))
                self.assertEqual(self.kube.events.count(("patch", 1)), 1)

    def test_lost_fence_response_still_fences_and_refuses_the_late_scale(self):
        first = self.process()
        first.state = first.check()
        self.kube.hold_scale = True
        self.kube.lose_fence = 99
        with self.assertRaisesRegex(RuntimeError, "could not be fenced|could not be confirmed"):
            first.quiesce(first.state)
        self.assertGreaterEqual(self.record()["fence"], 1)  # fence применился, ответ потерян
        held = len(self.kube.late)
        self.assertEqual(self.kube.deliver_late(), [False] * held)
        self.assertEqual(self.dep["spec"]["replicas"], 1)
        first.release()
        self.assertEqual((self.dep["spec"]["replicas"], self.record()), (1, None))

    def test_unconfirmed_fence_is_not_a_fence(self):
        # fence не применился и ответа нет: нельзя считать запоздавший scale мёртвым.
        first = self.process()
        first.state = first.check()
        self.kube.hold_scale = True
        original = self.kube.patch

        def deaf(args):
            body = json.loads(args[args.index("-p") + 1])
            note = body["metadata"].get("annotations", {}).get(self.NOTE)
            if note and json.loads(note)["fence"] > 0:
                return 1, "", "connection refused"
            return original(args)
        self.kube.patch = deaf
        with self.assertRaisesRegex(RuntimeError, "could not be fenced"):
            first.quiesce(first.state)

    def test_sigkill_at_every_write_is_settled_by_the_next_process(self):
        # Процесс исчезает перед n-й записью или сразу после её применения, дальше - второй процесс.
        for n in range(1, 3):  # намерение и scale: больше записей quiesce не делает
            for applied in (False, True):
                with self.subTest(write=n, applied=applied):
                    self.setUp()
                    original, count = self.kube.patch, []

                    def killer(args, original=original, count=count, n=n, applied=applied):
                        if "--dry-run=server" in args or "lease" in args:
                            return original(args)
                        count.append(1)
                        if len(count) < n:
                            return original(args)
                        if applied:
                            original(args)
                        raise SystemExit("SIGKILL")
                    self.kube.patch = killer
                    self.operator_clears_lease()  # прошлый круг цикла: процесс мёртв, Lease снята
                    first = self.process()
                    first.state = first.check()
                    with self.assertRaises(SystemExit):
                        first.quiesce(first.state)
                    self.kube.patch = original
                    self.operator_clears_lease()
                    second = self.process(now=5000.0)
                    try:
                        second.recover()
                    except RuntimeError as error:
                        self.assertRegex(str(error), "replicas=1 was restored")
                    self.assertEqual((self.dep["spec"]["replicas"], self.record()), (1, None))
                    self.assertEqual(self.dep["spec"]["template"]["spec"]["containers"][0]["image"], IMAGE)

    def test_sigkill_during_the_restore_itself_is_settled_again(self):
        first = self.process()
        first.state = first.check()
        first.quiesce(first.state)
        original = self.kube.patch

        def killer(args):
            if "lease" in args:
                return original(args)
            original(args)  # restore применён, процесс исчез до ответа
            raise SystemExit("SIGKILL")
        self.kube.patch = killer
        with self.assertRaises(SystemExit):
            self.recovered_by_next(5000)
        self.kube.patch = original
        self.assertEqual((self.dep["spec"]["replicas"], self.record()), (1, None))
        self.assertIsNone(self.recovered_by_next(6000))

    def test_lost_restore_response_keeps_a_single_replicas_write(self):
        self.kube.lose_restore = 1
        first = self.process()
        first.state = first.check()
        first.quiesce(first.state)
        with self.assertRaisesRegex(RuntimeError, "replicas=1 was restored"):
            self.recovered_by_next(5000)
        self.assertEqual(self.kube.events.count(("patch", 1)), 1)

    def test_apply_that_never_converges_gives_up_with_the_manual_procedure(self):
        first = self.process()
        first.state = first.check()
        first.quiesce(first.state)
        self.kube.on_patch = lambda cluster: cluster.bump()  # каждый CAS проигрывает гонку
        with self.assertRaisesRegex(RuntimeError, "may be left at replicas=0.*docs/DEPLOY.md"):
            first.release()
        self.assertEqual(self.dep["spec"]["replicas"], 0)


# FEATURE: Helper failure is reported immediately with a safe, bounded cause (#220).
# SPEC: both terminal phases end the wait; the message holds only whitelisted
# reason, exit code and a fixed-grammar log match; UID cleanup still runs.
class ForcedChainTests(IsolatedTestCase):
    """Штатная цепочка deploy/brood-deploy.sh: реальные `--check`, затем реальный `run` над
    FakeKubernetes. Команды берутся из самого скрипта, а не повторяются руками, поэтому
    порядок check -> run проверяется тем, что исполняется на машине деплоя."""

    NOTE = "brood.dev/quiesce"

    def setUp(self):
        from brood.dev.deploy.existing_install import ExistingInstall
        self.suite = ExclusiveWriterTests("test_foreign_pods_mounting_the_claim_are_refused_before_scale_or_helper")
        self.suite.setUp()
        self.deploy, self.rollout, self.kube = CrashLoopRecoveryTests.real_flow(self.suite)
        del self.deploy.check  # настоящий preflight, не заглушка real_flow
        self.dep = self.kube.objects["deployment", "brood-go"]
        self.now = 1000.0
        self.real = ExistingInstall
        self.verify_ok = True
        raw = self.kube.output

        def output(*args):
            if "logs" in args and "--previous" not in args and not self.verify_ok \
                    and self.kube.created["pod"]["spec"]["containers"][0]["command"][2] == "verify":
                return 0, json.dumps({"schema_version": 16, "target_version": 16, "verified": False}), ""
            return raw(*args)

        self.deploy.cluster.shell.output.side_effect = output

    def commands(self):
        text = (Path(__file__).resolve().parents[4] / "deploy" / "brood-deploy.sh").read_text()
        found = [line.split()[3:] for line in text.splitlines() if line.startswith("bin/brood deploy server")]
        self.assertEqual(found, [["--check"], []])
        return found

    def chain(self):
        """Выполняет команды скрипта по порядку; первая неудача обрывает цепочку (errexit)."""
        command = __import__("brood.dev.deploy.cli.server", fromlist=["main"])
        ran = []
        with patch.object(public, "Infrastructure"), patch.object(public, "GoDeploy") as go, \
                patch.object(command, "settings"), patch.object(command, "cluster"), \
                patch.object(command, "Deployment", return_value=self.deploy), \
                patch.object(public, "ExistingInstall",
                             side_effect=lambda d, **k: self.process(d, **k)), \
                patch("sys.stdout", new_callable=io.StringIO) as out:
            go.return_value.run.side_effect = self.rollout
            for argv in self.commands():
                ran.append(argv)
                try:
                    command.main(argv)
                except RuntimeError as error:
                    return ran, error, out.getvalue()
        return ran, None, out.getvalue()

    def process(self, deploy=None, **options):
        return self.track(self.real(deploy or self.deploy, wall=lambda: self.now, **FakeTime().options(), **options))

    def kill_after_scale(self):
        first = self.process()
        first.state = first.check()
        first.quiesce(first.state)  # SIGKILL: release не вызывался, Lease осталась
        first.unlock()
        self.assertEqual(self.dep["spec"]["replicas"], 0)
        self.now += 300

    def writes(self):
        return [e for e in self.kube.events if e[0] in {"patch", "annotate", "create", "apply", "logs"}]

    def test_check_reports_recoverable_without_writing_and_run_restores(self):
        self.kill_after_scale()
        before = len(self.kube.events)
        ran, error, _ = self.chain()
        # Lease мёртвого запуска: цепочка стоит на --check, ничего не пишет и не ждёт.
        self.assertEqual(ran, [["--check"]])
        self.assertRegex(str(error), "lease/brood-go-deploy exists")
        self.assertEqual(len(self.kube.events), before)
        self.operator_clears_lease()
        ran, error, _ = self.chain()
        # --check пропустил запись и ничего не писал; run восстановил replicas и остановился.
        self.assertEqual(ran, [["--check"], []])
        self.assertRegex(str(error), "replicas=1 was restored.*rerun")
        self.assertEqual(self.dep["spec"]["replicas"], 1)
        self.assertIsNone(self.dep["metadata"].get("annotations", {}).get(self.NOTE))
        self.assertEqual([e for e in self.kube.events[before:] if e[0] != "probe"], [("patch", 1)])
        self.assertNotIn(("apply",), self.kube.events)
        # Следующая штатная цепочка идёт обычным crash-loop recovery.
        ran, error, out = self.chain()
        self.assertIsNone(error)
        self.assertIn("Recovered an interrupted rollout", out)
        kinds = [e for e in self.kube.events if e[0] in {"logs", "apply"}][-4:]
        self.assertEqual(kinds, [("logs", "snapshot"), ("logs", "verify"), ("apply",), ("logs", "inspect")])

    def test_check_alone_never_writes_on_a_recoverable_record(self):
        self.kill_after_scale()
        events = len(self.kube.events)
        command = __import__("brood.dev.deploy.cli.server", fromlist=["main"])
        with patch.object(public, "Infrastructure"), patch.object(public, "GoDeploy"), \
                patch.object(command, "settings"), patch.object(command, "cluster"), \
                patch.object(command, "Deployment", return_value=self.deploy), \
                patch.object(public, "ExistingInstall", side_effect=lambda d, **k: self.process(d, **k)):
            with self.assertRaisesRegex(RuntimeError, "lease/brood-go-deploy exists"):
                command.main(["--check"])
            self.operator_clears_lease()
            self.assertEqual(command.main(["--check"]), 0)
        self.assertEqual(len(self.kube.events), events)
        self.assertEqual(self.dep["spec"]["replicas"], 0)

    def test_young_record_stops_the_chain_at_check_with_no_writes(self):
        self.kill_after_scale()
        self.operator_clears_lease()
        self.now -= 280
        events = len(self.kube.events)
        ran, error, _ = self.chain()
        self.assertEqual(ran, [["--check"]])
        self.assertRegex(str(error), "only .*s old.*nothing was changed")
        self.assertEqual(len(self.kube.events), events)
        self.assertEqual(self.dep["spec"]["replicas"], 0)
        self.assertIsNotNone(self.dep["metadata"]["annotations"][self.NOTE])

    def test_foreign_changes_or_garbage_stop_the_chain_at_check(self):
        self.kill_after_scale()
        self.operator_clears_lease()
        image = "localhost:5002/brood-go@sha256:" + "e" * 64
        self.dep["spec"]["template"]["spec"]["containers"][0]["image"] = image
        self.dep["metadata"]["generation"] += 1
        events = len(self.kube.events)
        for mutate in (lambda: None, lambda: self.dep["metadata"]["annotations"].update({self.NOTE: "not json"}),
                       lambda: self.dep["metadata"].update(uid="other")):
            mutate()
            ran, error, _ = self.chain()
            self.assertEqual(ran, [["--check"]])
            self.assertIsInstance(error, RuntimeError)
        self.assertEqual(len(self.kube.events), events)
        self.assertEqual(self.dep["spec"]["replicas"], 0)

    def test_stale_intent_is_cleared_by_run_and_deploy_continues(self):
        first = self.process()
        first.state = first.check()
        original, in_flight = self.kube.patch, []

        def intercept(args):
            if json.loads(args[args.index("-p") + 1]).get("spec", {}).get("replicas") == 0:
                in_flight.append(args)
                raise SystemExit("SIGKILL")
            return original(args)

        self.kube.patch = intercept
        with self.assertRaises(SystemExit):
            first.quiesce(first.state)
        self.kube.patch = original
        self.assertEqual(self.dep["spec"]["replicas"], 1)
        self.now += 300
        self.operator_clears_lease(first)
        ran, error, out = self.chain()
        self.assertIsNone(error)
        self.assertIsNone(self.dep["metadata"].get("annotations", {}).get(self.NOTE))
        self.assertIn(("apply",), self.kube.events)

    def test_verifier_failure_after_recovery_never_applies_and_restores_replicas(self):
        self.kill_after_scale()
        self.operator_clears_lease()
        self.assertIsNotNone(self.chain()[1])  # первый проход только восстанавливает
        self.verify_ok = False
        ran, error, _ = self.chain()
        self.assertEqual(ran, [["--check"], []])
        self.assertIsInstance(error, RuntimeError)
        self.assertNotIn(("apply",), self.kube.events)
        self.assertEqual(self.dep["spec"]["replicas"], 1)
        self.assertIsNone(self.dep["metadata"].get("annotations", {}).get(self.NOTE))

    def test_lost_responses_inside_the_chain_still_settle(self):
        self.kube.lose_intent = 1
        self.kube.fail_scale_after = 1
        ran, error, _ = self.chain()
        self.assertIsNone(error)
        self.assertEqual(self.kube.events.count(("patch", 0)), 1)
        self.assertNotIn(self.NOTE, self.dep["metadata"].get("annotations", {}))

    def test_without_the_live_verified_opt_in_nothing_is_scaled(self):
        self.deploy.get = {**VALUES, "BROOD_DEPLOY_MODE": "existing"}.get
        ran, error, _ = self.chain()
        self.assertEqual(ran, [["--check"]])
        self.assertRegex(str(error), "blocked")
        self.assertEqual(self.writes(), [])


class HelperFailureTests(IsolatedTestCase):
    def run_helper(self, phase, *, reason="Error", code=1, log="", never_terminal=False):
        from pathlib import Path
        from brood.dev.deploy.existing_install import ExistingInstall
        objects = resources()
        make_crashloop(objects)
        deployment = public.Deployment(Mock(), get={**VALUES, "BROOD_DEPLOY_MODE": "existing",
                                                    "BROOD_DEPLOY_SCALE_FENCE": "live-verified"}.get)
        created, deleted, now = {}, [], [0.0]

        def get_object(kind, name):
            if kind in created and name.startswith("brood-db-"):
                obj = copy.deepcopy(created[kind])
                if kind == "pod":
                    obj["status"] = {"phase": "Running" if never_terminal else phase, "containerStatuses": [
                        {"name": "database", "state": {"terminated": {"reason": reason, "exitCode": code}}}]}
                return obj
            return copy.deepcopy(objects.get((kind, name)))

        def output(*args):
            if "create" in args:
                obj = json.loads(Path(args[args.index("-f") + 1]).read_text())
                obj["metadata"]["uid"] = obj["kind"] + "-uid"
                created[obj["kind"].lower()] = obj
                return 0, json.dumps(obj), ""
            if "delete" in args:
                options = json.loads(Path(args[args.index("-f") + 1]).read_text())
                deleted.append(options["preconditions"]["uid"])
                return 0, "", ""
            if "wait" in args:
                return 0, "", ""
            if "logs" in args and "--previous" in args:
                return 0, SCHEMA_LOG, ""
            if "logs" in args:
                return 0, log, ""
            raise AssertionError(args)

        deployment.get_object = Mock(side_effect=get_object)
        deployment.cluster.shell.output.side_effect = output

        def sleep(seconds):
            now[0] += seconds

        helper = ExistingInstall(deployment, clock=lambda: now[0], sleep=sleep)
        helper.state = helper.check()
        helper.snapshot_path = "/data/.brood-snapshot-x.db"
        helper.before = {"sha256": "a" * 64}
        return helper, deleted, now

    def test_failed_phase_ends_wait_at_once_with_safe_cause_and_uid_cleanup(self):
        secret = "password=hunter2 token=abc private-row"
        log = secret + "\nincompatible database schema: table swarms differs from current schema\n"
        helper, deleted, now = self.run_helper("Failed", log=log)
        with self.assertRaises(RuntimeError) as raised:
            helper.execute(IMAGE, "verify")
        message = str(raised.exception)
        self.assertIn("verify failed", message)
        self.assertIn("reason=Error", message)
        self.assertIn("exit_code=1", message)
        self.assertIn("table swarms differs from current schema", message)
        self.assertNotIn("hunter2", message)
        self.assertNotIn("private-row", message)
        self.assertEqual(now[0], 0.0)
        self.assertEqual(deleted, ["Pod-uid", "NetworkPolicy-uid"])

    def test_every_schema_refusal_of_the_go_store_is_named_only_for_verify(self):
        # Полная грамматика отказов store: таблица, индекс, триггер, версия и две проверки данных.
        causes = ("table swarms differs from current schema", "missing table swarm_steps",
                  "index swarms_live_name differs from current schema", "missing index managed_pools_live_name",
                  "trigger swarm_graph_immutable differs from current schema",
                  "missing trigger pipeline_sources_no_delete", "found 15, want 16",
                  "swarm primary owner has no owner grant", "workflow revision counters differ from definitions",
                  "found 27, want 26; the database is newer than this release",
                  "found 15, want 27; no migration from version 15", "migration from version 26 to 27 failed")
        for cause in causes:
            with self.subTest(cause=cause):
                log = f"private-row\nbrood database: incompatible database schema: {cause}; recreate the database\n"
                helper, _, _ = self.run_helper("Failed", log=log)
                with self.assertRaises(RuntimeError) as raised:
                    helper.execute(IMAGE, "verify")
                message = str(raised.exception)
                self.assertIn("incompatible database schema: " + cause, message)
                self.assertIn("refused before apply", message)
                self.assertNotIn("private-row", message)
        helper, _, _ = self.run_helper("Failed", log="incompatible database schema: found 15, want 16\n")
        with self.assertRaises(RuntimeError) as raised:
            helper.execute(IMAGE, "snapshot")
        self.assertNotIn("refused before apply", str(raised.exception))

    def test_unknown_reason_and_unmatched_log_are_never_echoed(self):
        helper, _, _ = self.run_helper("Failed", reason="secret-reason-xyz", code=2, log="row: user=alice")
        with self.assertRaises(RuntimeError) as raised:
            helper.execute(IMAGE, "verify")
        message = str(raised.exception)
        self.assertIn("reason=unknown", message)
        self.assertNotIn("xyz", message)
        self.assertNotIn("alice", message)

    def test_succeeded_phase_returns_and_stuck_helper_times_out_inside_deadline(self):
        helper, deleted, _ = self.run_helper("Succeeded", log=json.dumps({"schema_version": 16, "target_version": 16, "verified": True}))
        self.assertTrue(helper.execute(IMAGE, "verify")["verified"])
        helper, deleted, now = self.run_helper("Running", never_terminal=True)
        with self.assertRaisesRegex(RuntimeError, "did not finish within 190s"):
            helper.execute(IMAGE, "verify")
        self.assertGreaterEqual(now[0], 190)
        self.assertEqual(deleted, ["Pod-uid", "NetworkPolicy-uid"])


# FEATURE: Fence compares the real Deployment generation and a second deploy process is excluded by a lease (#220).
# SPEC: NaN/Inf never parse; a generation moved by anyone but our single scale write blocks before
# apply (A->B->A, scale 1->0, annotation); a live process is never settled by a second one, and the
# decision does not rest on the record age alone (the worst live run outlasts GRACE).
# IMPLEMENTATION: ExistingInstall.parse_record / quiesce baseline / fence / claim / beat / drop_lease.
class GenerationFenceAndLeaseTests(IsolatedTestCase):
    NOTE = "brood.dev/quiesce"
    LEASE = ("lease", "brood-go-deploy")

    def setUp(self):
        self.suite = ExclusiveWriterTests("test_foreign_pods_mounting_the_claim_are_refused_before_scale_or_helper")
        self.suite.setUp()
        self.deploy, self.rollout, self.kube = CrashLoopRecoveryTests.real_flow(self.suite)
        self.dep = self.kube.objects["deployment", "brood-go"]

    def process(self, now=1000.0):
        from brood.dev.deploy.existing_install import ExistingInstall
        timing = FakeTime()
        install = ExistingInstall(self.deploy, wall=lambda: now, **timing.options())
        install.timing = timing
        return self.track(install)

    def scaled(self):
        first = self.process()
        first.state = first.check()
        first.quiesce(first.state)
        return first

    def external(self, **extra):
        meta = self.dep["metadata"]
        body = {"metadata": {"uid": meta["uid"], "resourceVersion": meta["resourceVersion"], **extra}}
        self.assertTrue(self.kube.apply_patch(body))

    def record(self):
        return json.loads(self.dep["metadata"]["annotations"][self.NOTE])

    # --- parse_record

    def test_record_with_nan_infinity_or_out_of_range_time_never_parses(self):
        first = self.process()
        base = json.dumps({"v": 1, "id": "a" * 32, "uid": "u", "phase": "intent", "image": "i", "pod": "p",
                           "spec": "b" * 64, "fence": 0, "at": 1000.0})
        self.assertEqual(first.parse_record(base, "u")["at"], 1000.0)
        for at in ("NaN", "Infinity", "-Infinity", "1e999", "-1e999", "-1", "true", '"1000"', "null", "1e308"):
            with self.subTest(at=at):
                with self.assertRaisesRegex(RuntimeError, "cannot be verified"):
                    first.parse_record(base.replace("1000.0", at), "u")

    def test_garbage_nonfinite_record_is_refused_by_check_and_recover_without_writes(self):
        self.dep["metadata"]["annotations"] = {self.NOTE: json.dumps({"at": 1}).replace("1}", "NaN}")}
        events = len(self.kube.events)
        with self.assertRaisesRegex(RuntimeError, "cannot be verified"):
            self.recovered_by_next(9999)
        self.assertEqual(len(self.kube.events), events)

    # --- generation

    def test_confirmed_scale_records_the_actual_generation_and_fence_passes(self):
        first = self.scaled()
        self.assertEqual(first.quiesced["generation"], self.dep["metadata"]["generation"])
        self.assertEqual(self.dep["metadata"]["generation"], 3)  # intent +1, scale +1
        first.fence()
        first.guard()

    def test_external_annotation_between_guards_blocks(self):
        first = self.scaled()
        self.external(annotations={"team.example/x": "1"})
        with self.assertRaisesRegex(RuntimeError, "generation moved"):
            first.guard()

    def test_a_b_a_spec_edit_with_identical_final_spec_blocks(self):
        first = self.scaled()
        container = self.dep["spec"]["template"]["spec"]["containers"][0]
        original = container["image"]
        for image in ("localhost:5002/brood-go@sha256:" + "7" * 64, original):
            container["image"] = image
            self.dep["metadata"]["generation"] += 1
            self.kube.bump()
        self.assertEqual(first.spec_digest(self.dep["spec"], 0), self.record()["spec"])
        with self.assertRaisesRegex(RuntimeError, "generation moved"):
            first.guard()

    def test_scale_up_and_down_by_someone_else_blocks(self):
        first = self.scaled()
        for replicas in (1, 0):
            self.dep["spec"]["replicas"] = replicas
            self.dep["metadata"]["generation"] += 1
            self.kube.bump()
        with self.assertRaisesRegex(RuntimeError, "generation moved"):
            first.guard()

    def test_generation_moving_before_the_helper_never_creates_it_or_applies(self):
        deploy, rollout, kube = self.deploy, self.rollout, self.kube

        def sneak(cluster):
            cluster.objects["deployment", "brood-go"]["metadata"]["generation"] += 1
            cluster.bump()
        kube.on_create = sneak
        with patch.object(public, "GoDeploy") as go:
            go.return_value.run.side_effect = rollout
            with self.assertRaisesRegex(RuntimeError, "generation moved|identity changed"):
                deploy.run()
        self.assertNotIn(("apply",), kube.events)
        self.assertEqual(self.dep["spec"]["replicas"], 1)  # release всё равно вернул реплики

    def test_external_annotation_between_intent_and_scale_is_baked_into_the_baseline(self):
        original = self.kube.patch
        done = []

        def meddle(args):
            if "-p" in args and "replicas" in args[args.index("-p") + 1] and not done:
                done.append(1)
                self.external(annotations={"team.example/x": "1"})  # RV сдвинулся: первый scale конфликтует
            return original(args)
        self.kube.patch = meddle
        first = self.scaled()
        self.assertEqual(first.quiesced["generation"], self.dep["metadata"]["generation"])
        first.fence()

    def test_lost_scale_reply_plus_external_write_right_after_cannot_be_proven(self):
        self.kube.fail_scale_after = 1
        self.kube.after_scale = lambda cluster: self.external(annotations={"team.example/x": "1"})
        first = self.process()
        first.state = first.check()
        with self.assertRaisesRegex(RuntimeError, "generation does not match"):
            first.quiesce(first.state)
        first.release()  # запись и replicas возвращает release по записи
        self.assertEqual(self.dep["spec"]["replicas"], 1)

    def test_lost_scale_reply_without_interference_is_proven_by_reading(self):
        self.kube.fail_scale_after = 1
        first = self.scaled()
        self.assertEqual(first.quiesced["generation"], self.dep["metadata"]["generation"])

    def test_reply_with_another_generation_is_not_trusted(self):
        original = self.kube.patch

        def liar(args):
            code, out, err = original(args)
            if code == 0 and "replicas" in args[args.index("-p") + 1]:
                data = json.loads(out)
                data["metadata"]["generation"] += 5
                out = json.dumps(data)
            return code, out, err
        self.kube.patch = liar
        with self.assertRaisesRegex(RuntimeError, "generation does not match"):
            self.scaled()

    # --- lease

    def lease(self):
        return self.kube.objects.get(self.LEASE)

    def manual_takeover(self):
        """Ручная процедура оператора из docs/DEPLOY.md: Lease заменена другой."""
        lease = self.lease()
        lease["spec"]["holderIdentity"] = "operator"
        self.kube.lease_rv += 1
        lease["metadata"]["resourceVersion"] = str(self.kube.lease_rv)

    def test_run_holds_a_renewed_lease_and_drops_it_after_release(self):
        first = self.scaled()
        self.assertEqual(self.lease()["spec"]["holderIdentity"], first.holder)
        first.guard()
        self.assertIn("patch", self.kube.lease_events)  # guard продлевает
        first.release()
        self.assertIsNone(self.lease())

    def test_hung_process_after_its_last_guard_cannot_be_displaced_by_age(self):
        """A завис после последнего guard, перед apply. Возраст аренды ничего не доказывает:
        B не берёт её, не ждёт и не пишет ни в Deployment, ни в Lease."""
        first = self.scaled()
        first.guard()  # последний guard A, дальше A "зависает"
        second = self.process(now=10 ** 7)
        second.timing.now = 10 ** 6
        events, leases = len(self.kube.events), list(self.kube.lease_events)
        with self.assertRaisesRegex(RuntimeError, "holds the deploy lock"):
            second.recover()  # A жив на этой машине: flock
        first.unlock()  # A завис на другой машине (или потерял flock): остаётся только Lease
        with self.assertRaisesRegex(RuntimeError, "lease/brood-go-deploy exists"):
            second.recover()
        with self.assertRaisesRegex(RuntimeError, "lease/brood-go-deploy exists"):
            second.snapshot(IMAGE)
        self.assertEqual((self.dep["spec"]["replicas"], len(self.kube.events)), (0, events))
        self.assertEqual(self.kube.lease_events, leases)
        self.assertEqual(second.timing.now, 10 ** 6)  # не ждал: наблюдение не подтверждение
        self.assertEqual(self.lease()["spec"]["holderIdentity"], first.holder)
        first.fence()  # поздний apply A всё ещё единственный писатель

    def test_sigkilled_process_leaves_a_lease_nobody_takes_over_automatically(self):
        first = self.scaled()
        first.unlock()  # SIGKILL: ядро снимает flock, Lease и запись остаются
        second = self.process(now=10 ** 7)
        events = len(self.kube.events)
        with self.assertRaisesRegex(RuntimeError, "lease/brood-go-deploy exists.*docs/DEPLOY.md"):
            second.recover()
        self.assertEqual((self.dep["spec"]["replicas"], len(self.kube.events)), (0, events))
        self.assertEqual(self.lease()["spec"]["holderIdentity"], first.holder)
        self.assertEqual(second.timing.now, 0.0)

    def test_second_process_on_the_same_machine_is_stopped_by_the_flock(self):
        first = self.process()
        first.claim()
        second = self.process()
        events = list(self.kube.lease_events)
        with self.assertRaisesRegex(RuntimeError, "another deploy process holds the deploy lock"):
            second.claim()
        self.assertEqual(self.kube.lease_events, events)
        first.release()
        second.claim()  # flock отпущен вместе с Lease
        second.release()

    def test_flock_is_released_by_the_kernel_with_the_process(self):
        script = ("import fcntl,sys;f=open(sys.argv[1],'a');fcntl.flock(f,fcntl.LOCK_EX);print('x',flush=True);"
                  "import time;time.sleep(60)")
        install = self.process()
        os.makedirs(os.path.dirname(install.lock_path), exist_ok=True)
        child = subprocess.Popen([sys.executable, "-I", "-c", script, install.lock_path], stdout=subprocess.PIPE)
        self.addCleanup(child.stdout.close)
        self.addCleanup(child.wait)
        self.addCleanup(child.kill)
        child.stdout.readline()
        with self.assertRaisesRegex(RuntimeError, "another deploy process holds the deploy lock"):
            install.claim()
        child.kill()
        child.wait()
        install.claim()
        install.release()

    def test_stalled_process_that_lost_its_lease_refuses_before_any_volume_access(self):
        first = self.scaled()
        self.manual_takeover()
        before = len(self.kube.events)
        with self.assertRaisesRegex(RuntimeError, "lease|generation|identity changed"):
            first.execute(IMAGE, "snapshot")
        self.assertNotIn(("create", "Pod"), self.kube.events[before:])

    def test_stalled_process_lease_taken_by_someone_else_is_detected_on_beat(self):
        first = self.scaled()
        lease = self.lease()
        lease["spec"]["holderIdentity"] = "other"
        self.kube.lease_rv += 1
        lease["metadata"]["resourceVersion"] = str(self.kube.lease_rv)
        with self.assertRaisesRegex(RuntimeError, "taken over"):
            first.beat(force=True)
        self.assertIsNone(first.lease)

    def test_lost_lease_is_sticky_every_later_call_refuses_without_writes(self):
        first = self.scaled()
        self.manual_takeover()
        with self.assertRaisesRegex(RuntimeError, "taken over"):
            first.guard()
        events, leases = len(self.kube.events), list(self.kube.lease_events)
        for call in (first.guard, first.guard, first.beat, first.fence,
                     lambda: first.execute(IMAGE, "snapshot"), first.settle, first.release):
            with self.subTest(call=call):
                with self.assertRaisesRegex(RuntimeError, "taken over"):
                    call()
        with self.assertRaisesRegex(RuntimeError, "taken over"):
            first.write(self.dep, None, replicas=1)
        self.assertEqual(len(self.kube.events), events)
        self.assertEqual(self.kube.lease_events, leases)

    def test_release_with_a_foreign_lease_writes_nothing_and_leaves_the_deployment(self):
        first = self.scaled()
        self.manual_takeover()
        before = json.dumps(self.dep, sort_keys=True)
        events, leases = len(self.kube.events), list(self.kube.lease_events)
        with self.assertRaisesRegex(RuntimeError, "docs/DEPLOY.md"):
            first.release()
        self.assertEqual(json.dumps(self.dep, sort_keys=True), before)
        self.assertEqual(self.dep["spec"]["replicas"], 0)
        self.assertEqual([e for e in self.kube.events[events:] if e[0] in ("patch", "delete")], [])
        self.assertEqual(self.kube.lease_events, leases)

    def test_recover_after_eviction_writes_nothing(self):
        first = self.scaled()
        self.manual_takeover()
        with self.assertRaisesRegex(RuntimeError, "taken over"):
            first.guard()
        before = json.dumps(self.dep, sort_keys=True)
        with self.assertRaisesRegex(RuntimeError, "taken over"):
            first.recover()
        self.assertEqual(json.dumps(self.dep, sort_keys=True), before)

    def test_heartbeat_is_periodic_by_the_process_clock(self):
        first = self.scaled()
        patches = self.kube.lease_events.count("patch")
        first.timing.now += first.BEAT - 1
        first.beat()
        self.assertEqual(self.kube.lease_events.count("patch"), patches)
        first.timing.now += 2
        first.beat()
        self.assertEqual(self.kube.lease_events.count("patch"), patches + 1)

    def test_helper_wait_renews_the_lease_through_the_whole_wait(self):
        first = self.scaled()
        pod = {"metadata": {"name": "h", "namespace": "brood", "uid": "u", "labels": {}}, "spec": {}}
        first.validate = lambda *a: None
        phases = iter(["Running"] * 30 + ["Succeeded"])
        self.deploy.get_object = Mock(side_effect=lambda kind, name: {**pod, "status": {"phase": next(phases)}})
        patches = self.kube.lease_events.count("patch")
        first.await_helper("snapshot", "h", pod, "u")
        self.assertGreaterEqual(self.kube.lease_events.count("patch") - patches, 2)  # 60 c ожидания, шаг 20 c

    def test_foreign_lease_is_never_taken(self):
        self.kube.objects[self.LEASE] = {"metadata": {"name": "brood-go-deploy", "namespace": "brood", "uid": "x",
                                                     "resourceVersion": "1", "labels": {}},
                                         "spec": {"holderIdentity": "x"}}
        with self.assertRaisesRegex(RuntimeError, "lease/brood-go-deploy exists"):
            self.process().claim()

    def test_foreign_lease_refuses_run_before_any_side_effect(self):
        self.kube.objects[self.LEASE] = {"metadata": {"name": "brood-go-deploy", "namespace": "brood", "uid": "x",
                                                     "resourceVersion": "1", "labels": {}},
                                         "spec": {"holderIdentity": "x"}}
        events = list(self.kube.events)
        with patch.object(public, "GoDeploy") as go:
            go.return_value.run.side_effect = self.rollout
            with self.assertRaisesRegex(RuntimeError, "lease/brood-go-deploy exists"):
                self.deploy.run()
        self.assertEqual((self.kube.events, self.kube.lease_events), (events, []))
        self.deploy.build_edge.assert_not_called()
        self.assertEqual(self.lease()["spec"]["holderIdentity"], "x")

    def test_check_alone_refuses_while_any_lease_exists_whatever_its_age(self):
        first = self.scaled()
        first.wall = lambda: 1400.0
        first.beat(force=True)
        for now in (1450.0, 1400.0 + 10 ** 6):
            with self.subTest(now=now):
                with self.assertRaisesRegex(RuntimeError, "lease/brood-go-deploy exists"):
                    self.process(now=now).check(recoverable=True)

    # --- healthy path и продолжительность Lease

    def test_healthy_run_holds_the_lease_through_apply_and_inspect_and_drops_it_last(self):
        deploy, rollout, kube = CrashLoopRecoveryTests.real_flow(self.suite, healthy=True)
        seen = []

        def watched(*, before_apply):
            seen.append(("before", kube.objects.get(self.LEASE) is not None))
            result = rollout(before_apply=before_apply)
            seen.append(("applied", kube.objects.get(self.LEASE) is not None))
            return result
        original = public.ExistingInstall.execute

        def execute(install, image, operation):
            seen.append((operation, kube.objects.get(self.LEASE) is not None))
            return original(install, image, operation)
        deploy.apply_edge.side_effect = lambda *a: seen.append(("edge", kube.objects.get(self.LEASE) is not None))
        with patch.object(public, "GoDeploy") as go, patch.object(public.ExistingInstall, "execute", execute):
            go.return_value.run.side_effect = watched
            deploy.run()
        self.assertTrue(seen and all(held for _, held in seen), seen)
        self.assertIn(("inspect", True), seen)
        self.assertIsNone(kube.objects.get(self.LEASE))

    def test_ambiguous_settle_inside_recover_keeps_the_lease_and_writes_nothing_more(self):
        self.scaled().unlock()
        self.operator_clears_lease()
        second = self.process(now=5000.0)
        real = second.settle

        def lost_reply(*args, **kwargs):
            self.kube.lose_restore = 1  # patch применился, ответ потерян
            real(*args, **kwargs)
            raise RuntimeError("deployment/brood-go restore could not be confirmed")
        second.settle = lost_reply
        with self.assertRaisesRegex(RuntimeError, "restore could not be confirmed.*docs/DEPLOY.md"):
            second.recover()
        events = len(self.kube.events)
        second.release()  # так делает run_existing при отказе
        self.assertIsNotNone(self.lease())
        self.assertEqual(self.lease()["spec"]["holderIdentity"], second.holder)
        self.assertEqual(len(self.kube.events), events)
        self.assertNotIn("delete", self.kube.lease_events)

    def test_unreadable_deployment_inside_recover_keeps_the_lease(self):
        self.scaled().unlock()
        self.operator_clears_lease()
        second = self.process(now=5000.0)
        original = second.deployment.get_object
        calls = []

        def unreadable(kind, name):
            if (kind, name) == ("deployment", "brood-go") and calls:
                return None
            calls.append(1)
            return original(kind, name)
        second.deployment.get_object = unreadable
        with self.assertRaisesRegex(RuntimeError, "cannot be read.*docs/DEPLOY.md"):
            second.recover()
        second.deployment.get_object = original
        second.release()
        self.assertIsNotNone(self.lease())

    def failing_delete(self, code=1):
        original = self.kube.output

        def output(*args):
            if "delete" in args and "--raw" in args:
                return code, "", "forbidden"
            return original(*args)
        self.kube.output = output
        self.deploy.cluster.shell.output.side_effect = output

    def test_failed_lease_delete_is_an_error_in_finish_and_release(self):
        for name in ("finish", "release"):
            with self.subTest(name=name):
                self.setUp()
                first = self.process()
                first.claim()
                self.failing_delete()
                with self.assertRaisesRegex(RuntimeError, "lease/brood-go-deploy.*docs/DEPLOY.md"):
                    getattr(first, name)()
                self.assertIsNotNone(self.lease())

    def test_release_text_never_claims_nothing_was_changed(self):
        first = self.scaled()
        first.settle = lambda **kw: (_ for _ in ()).throw(RuntimeError("deployment/brood-go cannot be read; " + first.PROCEDURE))
        with self.assertRaises(RuntimeError) as caught:
            first.release()
        self.assertNotIn("nothing was changed", str(caught.exception))
        self.assertIn("docs/DEPLOY.md", str(caught.exception))
        self.assertNotIn("nothing was changed", first.RELEASE_PROCEDURE)

    def test_lost_create_reply_does_not_claim_nothing_was_changed(self):
        first = self.process()
        first.lease_write = lambda current=None: None
        with self.assertRaises(RuntimeError) as caught:
            first.claim()
        self.assertNotIn("nothing was changed", str(caught.exception))
        self.assertIn("lease/brood-go-deploy", str(caught.exception))

    def test_lease_names_the_process_so_the_operator_can_prove_its_death(self):
        import socket
        with patch.dict(os.environ, {"BROOD_RELEASE_SHA": "a" * 40}):
            first = self.process()
            first.claim()
        lease = self.lease()
        notes = lease["metadata"]["annotations"]
        self.assertEqual(notes["brood.dev/hostname"], socket.gethostname())
        self.assertEqual(notes["brood.dev/pid"], str(os.getpid()))
        self.assertEqual(notes["brood.dev/release"], "a" * 40)
        self.assertRegex(notes["brood.dev/started"], r"^\d{4}-\d\d-\d\dT")

    def test_ambiguous_rollout_failure_keeps_the_lease_and_retries_nothing(self):
        deploy, rollout, kube = CrashLoopRecoveryTests.real_flow(self.suite, healthy=True)

        def broken(*, before_apply):
            before_apply(IMAGE)
            raise RuntimeError("Go deployment rollout failed: timeout")
        with patch.object(public, "GoDeploy") as go:
            go.return_value.run.side_effect = broken
            with self.assertRaisesRegex(RuntimeError, "rollout failed"):
                deploy.run()
        self.assertIsNotNone(kube.objects.get(self.LEASE))
        creates = kube.lease_events.count("create")
        with tempfile.TemporaryDirectory() as other_machine, patch.dict(os.environ, {"XDG_DATA_HOME": other_machine}):
            with patch.object(public, "GoDeploy") as go:
                go.return_value.run.side_effect = rollout
                with self.assertRaisesRegex(RuntimeError, "lease/brood-go-deploy exists"):
                    deploy.run()
        self.assertEqual(kube.lease_events.count("create"), creates)

    # FEATURE: a failure after apply names this deploy's snapshot (#274).
    # SPEC: the new server may already have migrated the database, so the
    # operator's rollback needs exactly the snapshot taken before apply; the
    # refusal carries its path and the rollback reference. Before apply the
    # running server and database are untouched and no snapshot is named.
    def test_failure_after_apply_names_the_snapshot_taken_before_apply(self):
        deploy, _, _ = CrashLoopRecoveryTests.real_flow(self.suite, healthy=True)

        def broken(*, before_apply):
            before_apply(IMAGE)
            raise RuntimeError("Go deployment rollout failed: timeout")
        with patch.object(public, "GoDeploy") as go:
            go.return_value.run.side_effect = broken
            with self.assertRaises(RuntimeError) as failure:
                deploy.run()
        text = str(failure.exception)
        self.assertRegex(text, r"rollout failed: timeout")
        self.assertRegex(text, r"/data/\.brood-snapshot-[0-9a-f]{32}\.db")
        self.assertIn("docs/DEPLOY.md", text)

    def test_failure_before_apply_names_no_snapshot(self):
        deploy, _, _ = CrashLoopRecoveryTests.real_flow(self.suite, healthy=True)

        def broken(*, before_apply):
            raise RuntimeError("Go image build failed")
        with patch.object(public, "GoDeploy") as go:
            go.return_value.run.side_effect = broken
            with self.assertRaises(RuntimeError) as failure:
                deploy.run()
        self.assertNotIn("snapshot", str(failure.exception))


# FEATURE: Database snapshots on the owned PVC are retained, not accumulated (#247).
# SPEC: with BROOD_DEPLOY_KEEP_SNAPSHOTS=N a helper of the new image, created the
# way snapshot helpers are (deny-all, no token, pinned digest, owned PVC), runs
# `brood database prune --current <this deploy's snapshot> --keep N` after the
# public probe and before the Lease is dropped; the reply must name this
# snapshot as kept. A failure is reported, never undoes the deploy, and still
# drops the Lease. Unset - no helper, previous behaviour.
# IMPLEMENTATION: ExistingInstall.retain over execute/manifests, public.run_existing.
class SnapshotRetentionTests(IsolatedTestCase):
    SNAPSHOT = "/data/.brood-snapshot-" + "d" * 32 + ".db"

    def helper(self):
        return ExistingInstallTests.helper(self)

    def test_prune_helper_is_writable_networkless_and_bound_to_this_snapshot(self):
        helper, _ = self.helper()
        state = helper.check()
        pod, policy = helper.manifests(IMAGE, state, "prune", "brood-db-test", self.SNAPSHOT, keep=3)
        container = pod["spec"]["containers"][0]
        self.assertEqual(container["command"], ["/brood", "database", "prune"])
        self.assertEqual(container["args"], ["--current", self.SNAPSHOT, "--keep", "3"])
        self.assertEqual(container["volumeMounts"], [{"name": "data", "mountPath": "/data", "readOnly": False}])
        self.assertEqual(container["image"], IMAGE)
        self.assertFalse(pod["spec"]["automountServiceAccountToken"])
        self.assertEqual((policy["spec"]["ingress"], policy["spec"]["egress"]), ([], []))
        for keep in (0, None, True, "3"):
            with self.subTest(keep=keep), self.assertRaisesRegex(RuntimeError, "at least one"):
                helper.manifests(IMAGE, state, "prune", "brood-db-test", self.SNAPSHOT, keep=keep)
        for destination in (None, "/data/brood.db", "/data/../etc/.brood-snapshot-x.db"):
            with self.subTest(destination=destination), self.assertRaisesRegex(RuntimeError, "destination"):
                helper.manifests(IMAGE, state, "prune", "brood-db-test", destination, keep=3)

    def test_reply_must_keep_this_snapshot_within_the_limit(self):
        helper, _ = self.helper()
        helper.snapshot_path = self.SNAPSHOT
        good = {"current": Path(self.SNAPSHOT).name, "kept": 3, "removed": 2}
        self.assertEqual(helper.retention(json.dumps(good), 3), good)
        for bad in ({**good, "current": ".brood-snapshot-" + "e" * 32 + ".db"}, {**good, "kept": 0},
                    {**good, "kept": 4}, {**good, "removed": -1}, {**good, "kept": True}, {**good, "extra": 1},
                    {k: v for k, v in good.items() if k != "removed"}, [good]):
            with self.subTest(bad=bad), self.assertRaisesRegex(RuntimeError, "this deploy's snapshot"):
                helper.retention(json.dumps(bad), 3)
        with self.assertRaisesRegex(RuntimeError, "this deploy's snapshot"):
            helper.retention("not json", 3)

    def test_retain_without_setting_or_snapshot_runs_no_helper(self):
        helper, _ = self.helper()
        helper.execute = Mock(side_effect=AssertionError("no helper"))
        self.assertEqual(helper.retain(IMAGE, None), {"enabled": False})
        result = helper.retain(IMAGE, 3)
        self.assertEqual(result["keep"], 3)
        self.assertIn("no verified snapshot", result["error"])

    def flow(self, keep="3"):
        deploy, rollout, kube = CrashLoopRecoveryTests.real_flow(self, healthy=True)
        values = {**VALUES, "BROOD_DEPLOY_MODE": "existing", "BROOD_ADMIN_PASSWORD": ""}
        if keep is not None:
            values["BROOD_DEPLOY_KEEP_SNAPSHOTS"] = keep
        deploy.get = values.get
        deploy.probe.side_effect = lambda *a, **k: kube.events.append(("public-probe",))
        leases = []
        kube.on_create = lambda k: leases.append((k.created["pod"]["spec"]["containers"][0]["command"][2],
                                                  ("lease", "brood-go-deploy") in k.objects))
        with patch.object(public, "GoDeploy") as go:
            go.return_value.run.side_effect = rollout
            result = deploy.run()
        return result, kube, leases

    def test_prune_runs_after_the_public_probe_under_the_lease_for_this_snapshot(self):
        result, kube, leases = self.flow()
        kinds = [e for e in kube.events if e[0] in {"logs", "apply", "public-probe"}]
        self.assertEqual(kinds, [("logs", "snapshot"), ("logs", "verify"), ("apply",), ("logs", "inspect"),
                                 ("public-probe",), ("logs", "prune")])
        self.assertEqual(kube.created["pod"]["spec"]["containers"][0]["args"],
                         ["--current", result["snapshot"], "--keep", "3"])
        self.assertEqual(leases[-1], ("prune", True))
        self.assertIsNone(kube.objects.get(("lease", "brood-go-deploy")))
        self.assertEqual(len(kube.deleted), 8)  # policy и pod каждого из четырёх helper - по UID
        self.assertEqual(result["snapshots"], {"enabled": True, "keep": 3, "current": Path(result["snapshot"]).name,
                                               "kept": 3, "removed": 2})

    def test_unset_setting_keeps_the_previous_flow(self):
        result, kube, _ = self.flow(keep=None)
        self.assertNotIn(("logs", "prune"), kube.events)
        self.assertEqual(len(kube.deleted), 6)
        self.assertEqual(result["snapshots"], {"enabled": False})

    def test_failed_or_dishonest_prune_is_reported_without_failing_the_deploy(self):
        for case in ("failed", "dishonest"):
            with self.subTest(case=case):
                deploy, rollout, kube = CrashLoopRecoveryTests.real_flow(self, healthy=True)
                deploy.get = {**VALUES, "BROOD_DEPLOY_MODE": "existing", "BROOD_ADMIN_PASSWORD": "",
                              "BROOD_DEPLOY_KEEP_SNAPSHOTS": "2"}.get
                if case == "failed":
                    kube.refuse["prune"] = "brood database: cannot remove an old snapshot\n"
                else:
                    kube.prune_reply = {"current": ".brood-snapshot-" + "f" * 32 + ".db", "kept": 1, "removed": 9}
                with patch.object(public, "GoDeploy") as go:
                    go.return_value.run.side_effect = rollout
                    result = deploy.run()
                self.assertEqual(result["acceptance"], "operator-authenticated acceptance pending")
                expected = "cannot remove an old snapshot" if case == "failed" else "this deploy's snapshot"
                self.assertIn(expected, result["snapshots"]["error"])
                self.assertIsNone(kube.objects.get(("lease", "brood-go-deploy")))
                self.assertEqual(len(kube.deleted), 8)

    def test_helper_integrity_failure_is_a_post_apply_failure_and_keeps_the_lease(self):
        # Не отказ самого prune: helper с записываемым томом мог остаться. Как у inspect -
        # отказ выкладки, Lease остаётся оператору (docs/DEPLOY.md, ручное снятие Lease).
        deploy, rollout, kube = CrashLoopRecoveryTests.real_flow(self, healthy=True)
        deploy.get = {**VALUES, "BROOD_DEPLOY_MODE": "existing", "BROOD_ADMIN_PASSWORD": "",
                      "BROOD_DEPLOY_KEEP_SNAPSHOTS": "2"}.get
        raw = kube.output

        def output(*args):
            if ("delete" in args and "/pods/" in args[args.index("--raw") + 1]
                    and kube.created["pod"]["spec"]["containers"][0]["command"][2] == "prune"):
                return 1, "", "etcdserver: request timed out"
            return raw(*args)

        deploy.cluster.shell.output.side_effect = output
        with patch.object(public, "GoDeploy") as go:
            go.return_value.run.side_effect = rollout
            with self.assertRaisesRegex(RuntimeError, "owned database helper failed"):
                deploy.run()
        self.assertIn(("apply",), kube.events)
        self.assertIsNotNone(kube.objects.get(("lease", "brood-go-deploy")))

    def test_cli_reports_retention_or_warns(self):
        command = __import__("brood.dev.deploy.cli.server", fromlist=["main"])
        base = {"image": IMAGE, "host": "h.example", "acceptance": "pending", "snapshot": self.SNAPSHOT}
        cases = {"enabled": ({"enabled": True, "keep": 3, "current": Path(self.SNAPSHOT).name, "kept": 3,
                              "removed": 2},
                             "Snapshot retention: kept 3 of BROOD_DEPLOY_KEEP_SNAPSHOTS=3 including this deploy's "
                             "snapshot, removed 2"),
                 "unset": ({"enabled": False}, "warning: snapshot retention skipped: BROOD_DEPLOY_KEEP_SNAPSHOTS is "
                                               "not set"),
                 "error": ({"enabled": True, "keep": 3, "error": "boom"},
                           "warning: snapshot retention failed: boom; older snapshots may remain on the owned PVC")}
        for name, (snapshots, line) in cases.items():
            with self.subTest(name), patch.object(command, "settings"), patch.object(command, "cluster"), \
                    patch.object(command, "Deployment") as deploy, patch("sys.stdout", new_callable=io.StringIO) as out:
                deploy.return_value.run.return_value = {**base, "snapshots": snapshots}
                command.main([])
            self.assertIn(line, out.getvalue())


# FEATURE: The forced command reaches the disk guard without a new SSH action (#247).
# SPEC: the commands of deploy/brood-deploy.sh, run in order with all three
# settings: --check measures space read-only; the run measures it before the
# Lease, prunes snapshots under the Lease after the probe, cleans Docker after
# the Lease is dropped, and prints each result.
class ForcedChainDiskGuardTests(IsolatedTestCase):
    SERVER_TAG = "localhost:5002/brood-go:build-" + "1" * 32
    EDGE_TAG = "localhost:5002/brood-go-edge:build-" + "2" * 32

    def setUp(self):
        from brood.dev.deploy import disk
        from brood.dev.deploy.tests.test_disk import FakeShell, df, image
        self.deploy, self.rollout, self.kube = CrashLoopRecoveryTests.real_flow(self, healthy=True)
        del self.deploy.check  # настоящий preflight
        self.deploy.get = {**VALUES, "BROOD_DEPLOY_MODE": "existing", "BROOD_ADMIN_PASSWORD": "",
                           disk.MIN_FREE: "10", disk.BUILD_CACHE: "5", disk.KEEP_SNAPSHOTS: "3"}.get
        self.deploy.build_edge = Mock(side_effect=lambda: setattr(self.deploy, "edge_tag", self.EDGE_TAG)
                                      or "localhost:5002/brood-go-edge@sha256:" + "e" * 64)
        listing = "\n".join([
            image("localhost:5002/brood-go", self.SERVER_TAG.rpartition(":")[2], "id-1", "2026-11-11 10:00:00 +0000 UTC"),
            image("localhost:5002/brood-go", "build-" + "3" * 32, "id-3", "2026-11-10 10:00:00 +0000 UTC"),
            image("localhost:5002/brood-go", "build-" + "4" * 32, "id-4", "2026-11-09 10:00:00 +0000 UTC"),
            image("localhost:5002/brood-go-edge", self.EDGE_TAG.rpartition(":")[2], "id-e", "2026-11-01 10:00:00 +0000 UTC"),
            image("localhost:5002/ateworker", "build-" + "5" * 32, "id-w", "2026-01-01 10:00:00 +0000 UTC")])
        machine = FakeShell({("docker", "info"): (0, "/var/lib/docker\n", ""), ("df",): (0, df(40 << 20), ""),
                             ("docker", "image", "ls"): (0, listing, ""), ("docker", "image", "rm"): (0, "", ""),
                             ("docker", "builder", "prune"): (0, "Total:\t55.2GB\n", "")})
        raw = self.kube.output

        def output(*args):
            if args[0] in {"docker", "df"}:
                self.kube.events.append(("machine", " ".join(args[:3]), ("lease", "brood-go-deploy") in self.kube.objects))
                return machine.output(*args)
            return raw(*args)

        self.deploy.cluster.shell.output.side_effect = output
        self.helpers = []
        self.kube.on_create = lambda k: self.helpers.append(
            (k.created["pod"]["spec"]["containers"][0]["command"][2], ("lease", "brood-go-deploy") in k.objects))

    def chain(self):
        command = __import__("brood.dev.deploy.cli.server", fromlist=["main"])
        text = (Path(__file__).resolve().parents[4] / "deploy" / "brood-deploy.sh").read_text()
        found = [line.split()[3:] for line in text.splitlines() if line.startswith("bin/brood deploy server")]
        self.assertEqual(found, [["--check"], []])
        outputs = []
        with patch.object(public, "Infrastructure"), patch.object(public, "GoDeploy") as go, \
                patch.object(command, "settings"), patch.object(command, "cluster"), \
                patch.object(command, "Deployment", return_value=self.deploy):
            go.return_value.tag = self.SERVER_TAG
            go.return_value.run.side_effect = self.rollout
            for argv in found:
                with patch("sys.stdout", new_callable=io.StringIO) as out:
                    command.main(argv)
                outputs.append(out.getvalue())
        return outputs

    def test_script_chain_guards_space_retains_snapshots_and_cleans_docker(self):
        check, run = self.chain()
        self.assertIn("Free space: 40.0 GiB on / (Docker root /var/lib/docker), required 10.0 GiB", check)
        steps = [e for e in self.kube.events if e[0] in {"machine", "logs", "apply"}]
        self.assertEqual(steps, [
            ("machine", "docker info --format", False), ("machine", "df -P -k", False),  # --check
            ("machine", "docker info --format", False), ("machine", "df -P -k", False),  # run: до Lease
            ("logs", "snapshot"), ("logs", "verify"), ("apply",), ("logs", "inspect"), ("logs", "prune"),
            ("machine", "docker image ls", False), ("machine", "docker image rm", False),
            ("machine", "docker builder prune", False)])
        self.assertEqual(self.helpers[-1], ("prune", True))
        self.assertIn("Snapshot retention: kept 3 of BROOD_DEPLOY_KEEP_SNAPSHOTS=3 including this deploy's "
                      "snapshot, removed 2", run)
        self.assertIn(f"Docker cleanup: removed 1 old release image tags, kept {self.SERVER_TAG}, "
                      f"localhost:5002/brood-go:build-{'3' * 32}, {self.EDGE_TAG}; build cache pruned to 5 GiB, "
                      "reclaimed 55.2GB", run)
        self.assertNotIn("warning", check + run)
