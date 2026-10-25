"""API сущностей Substrate: protojson в обе стороны, скоуп проекта из пути,
перевод отказов grpc с причиной ateapi, права и CSRF."""
from unittest import mock

import grpc
from django.contrib.auth.models import User
from django.test import TestCase
from django.utils.crypto import get_random_string
from google.protobuf.timestamp_pb2 import Timestamp

from brood.server.ateapi import ateapi_pb2 as pb
from brood.server.projects import client_of


def meta(name, atespace="", uid=None, version=1):
    created = Timestamp()
    created.FromJsonString("2026-10-24T10:00:00Z")
    return pb.ResourceMetadata(atespace=atespace, name=name, uid=uid or f"uid-{name}",
                               version=version, create_time=created, update_time=created)


def atespace(name):
    return pb.Atespace(metadata=meta(name))


def actor(name, project="brood", state=pb.ACTOR_STATE_RUNNING):
    return pb.Actor(metadata=meta(name, project),
                    actor_template=pb.ObjectRef(atespace=project, name="claude"),
                    status=pb.ActorStatus(state=state))


def template(name, project="brood"):
    return pb.ActorTemplate(metadata=meta(name, project),
                            containers=[pb.Container(name="claude", image="claude:1")])


def worker(name, state=pb.WORKER_STATE_ACTIVE):
    return pb.Worker(metadata=meta(name), worker_pool="pool-1",
                     status=pb.WorkerStatus(state=state))


def policy(version=5):
    return pb.AccessPolicy(metadata=meta("default", version=version),
                           bindings=[pb.Binding(role="viewer", members=["user:alice"])])


def rpc_error(code, details):
    """RpcError с кодом и причиной, как приносит grpc."""
    e = grpc.RpcError()
    e.code = lambda: grpc.StatusCode[code]
    e.details = lambda: details
    return e


class FakeClient:
    """ateapi в словарях: хватает, чтобы проверить прокси, не поднимая канал.
    error -- отказ любого вызова; calls -- изменяющие вызовы по порядку."""

    def __init__(self, atespaces=(), actors=(), templates=(), workers=(), policies=None,
                 error=None):
        self.atespaces = {a.metadata.name: a for a in atespaces}
        self.actors = {(a.metadata.atespace, a.metadata.name): a for a in actors}
        self.templates = {(t.metadata.atespace, t.metadata.name): t for t in templates}
        self.workers = {w.metadata.name: w for w in workers}
        self.policies = dict(policies or {})  # скоуп -> политика, "" -- глобальная
        self.error, self.calls = error, []

    def _find(self, store, key):
        if self.error:
            raise self.error
        if key not in store:
            raise rpc_error("NOT_FOUND", f"{key} not found")
        return store[key]

    def _record(self, *call):
        if self.error:
            raise self.error
        self.calls.append(call)

    def _scoped(self, store, project):
        if self.error:
            raise self.error
        return [v for (scope, _), v in store.items() if scope == project]

    # Atespace
    def list_atespaces(self):
        return self._scoped({("", k): v for k, v in self.atespaces.items()}, "")

    def get_atespace(self, name):
        return self._find(self.atespaces, name)

    def create_atespace(self, message):
        self._record("create_atespace", message)
        return message

    def delete_atespace(self, name):
        self._record("delete_atespace", name)

    # Actor
    def list_actors(self, project):
        return self._scoped(self.actors, project)

    def get_actor(self, project, name):
        return self._find(self.actors, (project, name))

    def create_actor(self, message):
        self._record("create_actor", message)
        return message

    def update_actor(self, message):
        self._record("update_actor", message)
        return message

    def suspend_actor(self, project, name):
        found = self._find(self.actors, (project, name))
        self._record("suspend_actor", project, name)
        found.status.state = pb.ACTOR_STATE_SUSPENDED
        return found

    def resume_actor(self, project, name):
        found = self._find(self.actors, (project, name))
        self._record("resume_actor", project, name)
        found.status.state = pb.ACTOR_STATE_RUNNING
        return found

    def delete_actor(self, project, name):
        self._record("delete_actor", project, name)

    # ActorTemplate
    def list_actor_templates(self, project):
        return self._scoped(self.templates, project)

    def get_actor_template(self, project, name):
        return self._find(self.templates, (project, name))

    def create_actor_template(self, message):
        self._record("create_actor_template", message)
        return message

    def delete_actor_template(self, project, name):
        self._record("delete_actor_template", project, name)

    # Worker
    def list_workers(self):
        return self._scoped({("", k): v for k, v in self.workers.items()}, "")

    def get_worker(self, name):
        return self._find(self.workers, name)

    def drain_worker(self, name):
        found = self._find(self.workers, name)
        self._record("drain_worker", name)
        found.status.state = pb.WORKER_STATE_DRAINING
        return found

    # AccessPolicy
    def get_global_access_policy(self):
        return self._find(self.policies, "")

    def create_global_access_policy(self, message):
        self._record("create_global_access_policy", message)
        return message

    def update_global_access_policy(self, message):
        self._record("update_global_access_policy", message)
        return message

    def get_atespace_access_policy(self, project):
        return self._find(self.policies, project)

    def create_atespace_access_policy(self, project, message):
        self._record("create_atespace_access_policy", project, message)
        return message

    def update_atespace_access_policy(self, project, message):
        self._record("update_atespace_access_policy", project, message)
        return message

    def delete_atespace_access_policy(self, project):
        self._record("delete_atespace_access_policy", project)


class ApiTestCase(TestCase):
    """Сессия суперпользователя; fake -- подменный клиент ateapi."""

    def setUp(self):
        self.client.force_login(User.objects.create_superuser("admin", "a@b.c", "pw"))

    def use(self, fake):
        patcher = mock.patch.object(client_of, "_testing", fake)
        patcher.start()
        self.addCleanup(patcher.stop)
        return fake

    def send(self, method, url, body=None):
        return getattr(self.client, method)(url, body, content_type="application/json")


class ProjectApiTests(ApiTestCase):
    def test_list_and_retrieve_in_protojson(self):
        self.use(FakeClient([atespace("brood"), atespace("sandbox")]))
        listed = self.client.get("/api/projects").json()["results"]
        got = self.client.get("/api/projects/brood").json()
        self.assertEqual([p["metadata"]["name"] for p in listed], ["brood", "sandbox"])
        self.assertEqual(got["metadata"]["createTime"], "2026-10-24T10:00:00Z")
        self.assertEqual(got["metadata"]["version"], "1")  # int64 в protojson -- строка

    def test_missing_carries_ateapi_reason(self):
        self.use(FakeClient())
        got = self.client.get("/api/projects/nope")
        self.assertEqual(got.status_code, 404)
        self.assertIn("nope", got.json()["detail"])

    def test_create(self):
        fake = self.use(FakeClient())
        got = self.send("post", "/api/projects", {"metadata": {"name": "brood"}})
        self.assertEqual(got.status_code, 201)
        self.assertEqual(fake.calls[0][1].metadata.name, "brood")

    def test_unknown_field_is_400_not_500(self):
        fake = self.use(FakeClient())
        got = self.send("post", "/api/projects", {"metadata": {"name": "x"}, "spec": {}})
        self.assertEqual(got.status_code, 400)
        self.assertIn("Atespace", got.json()["detail"])
        self.assertEqual(fake.calls, [])

    def test_delete_not_empty_keeps_ateapi_reason(self):
        """FAILED_PRECONDITION у ateapi -- не только «не пуст»: причину не
        подменяем своей."""
        self.use(FakeClient(error=rpc_error("FAILED_PRECONDITION", "Atespace brood is not empty")))
        got = self.client.delete("/api/projects/brood")
        self.assertEqual(got.status_code, 409)
        self.assertEqual(got.json()["detail"], "Atespace brood is not empty")

    def test_unavailable(self):
        self.use(FakeClient(error=rpc_error("UNAVAILABLE", "connection refused")))
        got = self.client.get("/api/projects")
        self.assertEqual(got.status_code, 503)
        self.assertEqual(got.json()["detail"], "connection refused")

    def test_anonymous_and_plain_user_denied(self):
        self.use(FakeClient())
        self.client.logout()
        self.assertEqual(self.client.get("/api/projects").status_code, 403)
        User.objects.create_user("plain", "p@b.c", "pw")
        self.client.login(username="plain", password="pw")
        self.assertEqual(self.client.get("/api/projects").status_code, 403)

    def test_write_needs_csrf_token(self):
        fake = self.use(FakeClient())
        self.client.handler.enforce_csrf_checks = True
        body = {"metadata": {"name": "brood"}}
        denied = self.send("post", "/api/projects", body)
        token = get_random_string(32)
        self.client.cookies["csrftoken"] = token
        ok = self.client.post("/api/projects", body, content_type="application/json",
                              HTTP_X_CSRFTOKEN=token)
        self.assertEqual((denied.status_code, ok.status_code), (403, 201))
        self.assertEqual(len(fake.calls), 1)


class MalformedRequestTests(ApiTestCase):
    """Отказ с причиной, а не 500: метод не той формы пути, тело не объект."""

    def test_method_on_the_wrong_path_shape_is_405(self):
        fake = self.use(FakeClient())
        cases = [("put", "/api/projects/brood/actors"), ("delete", "/api/projects/brood/actors"),
                 ("delete", "/api/projects"), ("post", "/api/projects/brood"),
                 ("post", "/api/projects/brood/templates/claude"), ("post", "/api/workers")]
        for method, url in cases:
            with self.subTest(method=method, url=url):
                got = self.send(method, url, {})
                self.assertEqual(got.status_code, 405)
                self.assertIn("not allowed", got.json()["detail"])
        self.assertEqual(fake.calls, [])

    def test_body_that_is_not_an_object_is_400(self):
        self.use(FakeClient(actors=[actor("luna")]))
        for url in ("/api/projects", "/api/projects/brood/actors/luna"):
            with self.subTest(url=url):
                got = self.send("post", url, ["not", "an", "object"])
                self.assertEqual(got.status_code, 400)


class ActorApiTests(ApiTestCase):
    def test_list_is_project_scoped_with_state_name(self):
        self.use(FakeClient(actors=[actor("luna"), actor("mars", "sandbox"),
                                    actor("parked", state=pb.ACTOR_STATE_SUSPENDED)]))
        listed = self.client.get("/api/projects/brood/actors").json()["results"]
        self.assertEqual([(a["metadata"]["name"], a["status"]["state"]) for a in listed],
                         [("luna", "ACTOR_STATE_RUNNING"), ("parked", "ACTOR_STATE_SUSPENDED")])

    def test_create_takes_project_from_path(self):
        fake = self.use(FakeClient())
        body = {"metadata": {"name": "luna", "atespace": "foreign"},
                "actorTemplate": {"atespace": "brood", "name": "claude"},
                "workerSelector": {"matchLabels": {"workload": "claude"}}}
        got = self.send("post", "/api/projects/brood/actors", body)
        sent = fake.calls[0][1]
        self.assertEqual(got.status_code, 201)
        self.assertEqual((sent.metadata.atespace, sent.metadata.name), ("brood", "luna"))
        self.assertEqual(dict(sent.worker_selector.match_labels), {"workload": "claude"})

    def test_update_takes_identity_from_path_and_keeps_preconditions(self):
        fake = self.use(FakeClient())
        body = {"metadata": {"name": "other", "uid": "uid-luna", "version": "7"},
                "actorTemplate": {"atespace": "brood", "name": "shell"}}
        got = self.send("put", "/api/projects/brood/actors/luna", body)
        sent = fake.calls[0][1]
        self.assertEqual(got.status_code, 200)
        self.assertEqual((sent.metadata.name, sent.metadata.uid, sent.metadata.version),
                         ("luna", "uid-luna", 7))
        self.assertEqual(sent.actor_template.name, "shell")

    def test_stale_update_is_conflict(self):
        self.use(FakeClient(error=rpc_error("ABORTED", "version mismatch")))
        got = self.send("put", "/api/projects/brood/actors/luna", {"metadata": {}})
        self.assertEqual(got.status_code, 409)

    def test_suspend_resume_and_unknown_action(self):
        fake = self.use(FakeClient(actors=[actor("luna")]))
        suspended = self.send("post", "/api/projects/brood/actors/luna", {"action": "suspend"})
        resumed = self.send("post", "/api/projects/brood/actors/luna", {"action": "Resume"})
        bad = self.send("post", "/api/projects/brood/actors/luna", {"action": "reboot"})
        self.assertEqual(suspended.json()["status"]["state"], "ACTOR_STATE_SUSPENDED")
        self.assertEqual(resumed.json()["status"]["state"], "ACTOR_STATE_RUNNING")
        self.assertEqual(bad.status_code, 400)
        self.assertEqual([c[0] for c in fake.calls], ["suspend_actor", "resume_actor"])

    def test_delete(self):
        fake = self.use(FakeClient())
        self.assertEqual(self.client.delete("/api/projects/brood/actors/luna").status_code, 204)
        self.assertEqual(fake.calls, [("delete_actor", "brood", "luna")])


class TemplateApiTests(ApiTestCase):
    def test_list_and_retrieve(self):
        self.use(FakeClient(templates=[template("claude"), template("shell", "sandbox")]))
        listed = self.client.get("/api/projects/brood/templates").json()["results"]
        got = self.client.get("/api/projects/brood/templates/claude").json()
        self.assertEqual([t["metadata"]["name"] for t in listed], ["claude"])
        self.assertEqual(got["containers"][0]["image"], "claude:1")

    def test_create_keeps_fields_the_form_does_not_know(self):
        """Панель шлёт объект целиком: тома, проба, политика снапшотов --
        всё доезжает до ateapi, а atespace ставит путь."""
        fake = self.use(FakeClient())
        body = {"metadata": {"name": "claude", "atespace": "foreign"},
                "containers": [{"name": "claude", "image": "claude:1",
                                "volumeMounts": [{"name": "work", "mountPath": "/work"}]}],
                "volumes": [{"name": "work", "durableDir": {}}],
                "resources": {"limits": {"cpuMillicores": "1000", "memoryBytes": "2147483648"}},
                "snapshotConfig": {"preferredFidelity": "SNAPSHOT_FIDELITY_MEMORY"}}
        got = self.send("post", "/api/projects/brood/templates", body)
        sent = fake.calls[0][1]
        self.assertEqual(got.status_code, 201)
        self.assertEqual(sent.metadata.atespace, "brood")
        self.assertTrue(sent.volumes[0].HasField("durable_dir"))
        self.assertEqual(sent.resources.limits.memory_bytes, 2147483648)
        self.assertEqual(sent.snapshot_config.preferred_fidelity, pb.SNAPSHOT_FIDELITY_MEMORY)

    def test_bad_type_is_400(self):
        self.use(FakeClient())
        got = self.send("post", "/api/projects/brood/templates",
                        {"resources": {"limits": {"cpuMillicores": "a lot"}}})
        self.assertEqual(got.status_code, 400)

    def test_delete(self):
        fake = self.use(FakeClient())
        self.assertEqual(self.client.delete("/api/projects/brood/templates/claude").status_code,
                         204)
        self.assertEqual(fake.calls, [("delete_actor_template", "brood", "claude")])


class WorkerApiTests(ApiTestCase):
    def test_list_and_drain(self):
        fake = self.use(FakeClient(workers=[worker("w1"),
                                            worker("w2", pb.WORKER_STATE_UNAVAILABLE)]))
        listed = self.client.get("/api/workers").json()["results"]
        drained = self.send("post", "/api/workers/w1", {"action": "drain"})
        bad = self.send("post", "/api/workers/w1", {"action": "cordon"})
        self.assertEqual([w["status"]["state"] for w in listed],
                         ["WORKER_STATE_ACTIVE", "WORKER_STATE_UNAVAILABLE"])
        self.assertEqual(drained.json()["status"]["state"], "WORKER_STATE_DRAINING")
        self.assertEqual(bad.status_code, 400)
        self.assertEqual(fake.calls, [("drain_worker", "w1")])


class PolicyApiTests(ApiTestCase):
    def test_get_global_and_missing_project_policy(self):
        self.use(FakeClient(policies={"": policy()}))
        got = self.client.get("/api/policy/global").json()
        missing = self.client.get("/api/policy/projects/brood")
        self.assertEqual(got["bindings"], [{"role": "viewer", "members": ["user:alice"]}])
        self.assertEqual(missing.status_code, 404)

    def test_create_project_policy(self):
        """Своей политики у проекта нет -- её заводит POST, а не PUT: Update
        несуществующей ateapi отклонит."""
        fake = self.use(FakeClient())
        body = {"bindings": [{"role": "editor", "members": ["user:bob"]}]}
        got = self.send("post", "/api/policy/projects/brood", body)
        name, project, sent = fake.calls[0]
        self.assertEqual(got.status_code, 201)
        self.assertEqual((name, project), ("create_atespace_access_policy", "brood"))
        self.assertEqual(sent.bindings[0].members, ["user:bob"])

    def test_update_forces_singleton_identity_and_keeps_preconditions(self):
        fake = self.use(FakeClient())
        body = {"metadata": {"name": "mine", "atespace": "brood", "uid": "u", "version": "5"},
                "bindings": [{"role": "owner", "members": ["user:root"]}]}
        self.send("put", "/api/policy/global", body)
        sent = fake.calls[0][1]
        self.assertEqual((sent.metadata.name, sent.metadata.atespace), ("default", ""))
        self.assertEqual((sent.metadata.uid, sent.metadata.version), ("u", 5))

    def test_role_check_is_ateapis(self):
        """Допустимые роли скоупа знает ateapi; его отказ доходит как есть."""
        self.use(FakeClient(error=rpc_error("INVALID_ARGUMENT",
                                            'bindings[0].role: Unsupported value: "editor"')))
        got = self.send("put", "/api/policy/global",
                        {"bindings": [{"role": "editor", "members": ["user:bob"]}]})
        self.assertEqual(got.status_code, 400)
        self.assertIn("editor", got.json()["detail"])

    def test_delete_project_policy_but_not_global(self):
        fake = self.use(FakeClient())
        project = self.client.delete("/api/policy/projects/brood")
        global_ = self.client.delete("/api/policy/global")
        self.assertEqual((project.status_code, global_.status_code), (204, 405))
        self.assertEqual(fake.calls, [("delete_atespace_access_policy", "brood")])
