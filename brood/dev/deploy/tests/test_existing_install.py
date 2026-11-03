"""Проверки выкладки существующей установки без operator credentials."""
import base64
import copy
import json
import unittest
from unittest.mock import Mock, patch

from brood.dev.deploy import public
from brood.dev.deploy.go_server import manifests
from brood.dev.deploy.tests.test_public import VALUES


IMAGE = "localhost:5002/brood-go@sha256:" + "a" * 64


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
    return result


# FEATURE: Explicit credential-free existing-install deployment.
# SPEC: Fail before mutation on unknown ownership/runtime; snapshot before rollout;
# never bootstrap/login; metadata only, no database contents or credentials in logs.
# IMPLEMENTATION: Owned bounded helper gate receives the built digest before apply.
# RESULT: IMPLEMENTED — fail-closed snapshot and post-rollout inspect, no auth mutation.
# STATUS: DONE
class ExistingInstallTests(unittest.TestCase):
    def helper(self, **options):
        from brood.dev.deploy.existing_install import ExistingInstall
        objects = resources(**options)
        deployment = public.Deployment(Mock(), get={**VALUES, "BROOD_DEPLOY_MODE": "existing"}.get)
        deployment.get_object = Mock(side_effect=lambda kind, name: copy.deepcopy(objects.get((kind, name))))
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
        helper.execute = Mock(return_value={"schema_version": 6, "enabled_admins": 1, "users": 1})
        helper.inspect(IMAGE)
        helper.execute.return_value = {"schema_version": 6, "enabled_admins": 1, "users": 2}
        with self.assertRaisesRegex(RuntimeError, "user|admin"):
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
        helper.deployment.get_object.side_effect = lambda kind, name: (
            copy.deepcopy(created[kind]) if kind in created else original(kind, name))
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
            deploy = go_server.Deploy(cluster, get=get)
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
        values = {**VALUES, "BROOD_DEPLOY_MODE": "existing", "BROOD_ADMIN_PASSWORD": ""}
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
