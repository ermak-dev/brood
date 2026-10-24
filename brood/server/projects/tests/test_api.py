"""API проектов и акторов: разметка Atespace/Actor, перевод отказов grpc,
права, CSRF и действия над актором."""
import unittest
from unittest import mock

from django.test import TestCase

from brood.server.ateapi import ateapi_pb2
from brood.server.projects import client_of, views


def atespace(name, uid="uid-1", version=3):
    """Atespace с отметками времени -- как отдаёт ateapi."""
    return ateapi_pb2.Atespace(
        metadata=ateapi_pb2.ResourceMetadata(
            name=name, uid=uid, version=version,
            create_time=_timestamp("2026-10-24T10:00:00Z"),
            update_time=_timestamp("2026-10-24T11:30:00Z")))


def actor(name, atespace_name="brood", state=ateapi_pb2.ACTOR_STATE_RUNNING):
    """Actor в состоянии state -- как отдаёт ateapi."""
    return ateapi_pb2.Actor(
        metadata=ateapi_pb2.ResourceMetadata(
            atespace=atespace_name, name=name, uid=f"actor-{name}", version=1,
            create_time=_timestamp("2026-10-24T10:00:00Z"),
            update_time=_timestamp("2026-10-24T11:30:00Z")),
        status=ateapi_pb2.ActorStatus(state=state))


def _timestamp(iso):
    from google.protobuf.timestamp_pb2 import Timestamp

    t = Timestamp()
    t.FromJsonString(iso)
    return t


class FakeClient:
    """Ответы и отказы ateapi по списку имен -- чтобы не поднимать канал."""

    def __init__(self, atespaces=(), actors=(), error=None):
        self.atespaces = list(atespaces)
        self.actors, self.error = list(actors), error
        self.created, self.deleted = [], []
        self.acted = []

    def list_atespaces(self):
        if self.error:
            raise self.error
        return self.atespaces

    def get_atespace(self, name):
        for a in self.atespaces:
            if a.metadata.name == name:
                return a
        raise self.error

    def create_atespace(self, name):
        if self.error:
            raise self.error
        self.created.append(name)
        return atespace(name)

    def delete_atespace(self, name):
        if self.error:
            raise self.error
        self.deleted.append(name)

    # ─── акторы ──────────────────────────────────────────────────────────
    def list_actors(self, atespace_name):
        if self.error:
            raise self.error
        return [a for a in self.actors if a.metadata.atespace == atespace_name]

    def get_actor(self, atespace_name, name):
        for a in self.actors:
            if a.metadata.atespace == atespace_name and a.metadata.name == name:
                return a
        raise self.error

    def suspend_actor(self, atespace_name, name):
        return self._act(atespace_name, name, "suspend")

    def resume_actor(self, atespace_name, name):
        return self._act(atespace_name, name, "resume")

    def delete_actor(self, atespace_name, name):
        if self.error:
            raise self.error
        self.acted.append(("delete", atespace_name, name))
        return None

    def _act(self, atespace_name, name, action):
        actor = self.get_actor(atespace_name, name)
        state = (ateapi_pb2.ACTOR_STATE_SUSPENDED if action == "suspend"
                 else ateapi_pb2.ACTOR_STATE_RUNNING)
        self.acted.append((action, atespace_name, name))
        actor.status.state = state
        return actor


def rpc_error(code_name, details="boom"):
    """RpcError с кодом, как приносит grpc."""
    import grpc

    e = grpc.RpcError()
    e.code = lambda: grpc.StatusCode.__members__[code_name]
    e.details = lambda: details
    return e


class ProjectsApiTests(TestCase):
    def setUp(self):
        from django.contrib.auth.models import User

        self.admin = User.objects.create_superuser("admin", "a@b.c", "pw")
        self.client.force_login(self.admin)

    def test_list(self):
        with mock.patch.object(client_of, "_testing",
                               FakeClient([atespace("brood"), atespace("sandbox")])):
            got = self.client.get("/api/projects")
        self.assertEqual(got.status_code, 200)
        self.assertEqual([p["name"] for p in got.json()["results"]], ["brood", "sandbox"])
        self.assertEqual(got.json()["results"][0]["create_time"], "2026-10-24T10:00:00+00:00")

    def test_retrieve_and_missing(self):
        fake = FakeClient([atespace("brood")], error=rpc_error("NOT_FOUND",
                                                               "no such atespace"))
        with mock.patch.object(client_of, "_testing", fake):
            self.assertEqual(self.client.get("/api/projects/brood").status_code, 200)
            got = self.client.get("/api/projects/other")
        self.assertEqual(got.status_code, 404)
        self.assertEqual(got.json()["detail"], "no such atespace")

    def test_create(self):
        fake = FakeClient()
        with mock.patch.object(client_of, "_testing", fake):
            got = self.client.post("/api/projects", {"name": "brood"})
        self.assertEqual(got.status_code, 201)
        self.assertEqual(fake.created, ["brood"])
        self.assertEqual(got.json()["name"], "brood")

    def test_create_without_name(self):
        fake = FakeClient()
        with mock.patch.object(client_of, "_testing", fake):
            got = self.client.post("/api/projects", {"name": "  "})
        self.assertEqual(got.status_code, 400)
        self.assertEqual(fake.created, [])

    def test_delete_not_empty(self):
        fake = FakeClient(error=rpc_error("FAILED_PRECONDITION"))
        with mock.patch.object(client_of, "_testing", fake):
            got = self.client.delete("/api/projects/brood")
        self.assertEqual(got.status_code, 409)
        self.assertEqual(got.json()["detail"], views.FAILED_PRECONDITION)
        self.assertEqual(fake.deleted, [])

    def test_delete(self):
        fake = FakeClient([atespace("brood")])
        with mock.patch.object(client_of, "_testing", fake):
            got = self.client.delete("/api/projects/brood")
        self.assertEqual(got.status_code, 204)
        self.assertEqual(fake.deleted, ["brood"])

    def test_anonymous_denied(self):
        self.client.logout()
        self.assertEqual(self.client.get("/api/projects").status_code, 403)

    def test_not_superuser_denied(self):
        from django.contrib.auth.models import User

        User.objects.create_user("plain", "p@b.c", "pw")
        self.client.login(username="plain", password="pw")
        self.assertEqual(self.client.get("/api/projects").status_code, 403)

    def test_post_needs_csrf_token(self):
        # Django test client выключает CSRF; включаем, как в браузере.
        from django.utils.crypto import get_random_string

        self.client.handler.enforce_csrf_checks = True
        fake = FakeClient()
        with mock.patch.object(client_of, "_testing", fake):
            denied = self.client.post("/api/projects", {"name": "brood"})
            token = get_random_string(32)
            self.client.cookies["csrftoken"] = token
            ok = self.client.post("/api/projects", {"name": "brood"},
                                  HTTP_X_CSRFTOKEN=token)
        self.assertEqual(denied.status_code, 403)
        self.assertEqual(ok.status_code, 201)

    def test_unavailable(self):
        fake = FakeClient(error=rpc_error("UNAVAILABLE", "connection refused"))
        with mock.patch.object(client_of, "_testing", fake):
            got = self.client.get("/api/projects")
        self.assertEqual(got.status_code, 503)
        self.assertIn("connection refused", got.json()["detail"])


class ActorApiTests(TestCase):
    def setUp(self):
        from django.contrib.auth.models import User

        self.admin = User.objects.create_superuser("admin", "a@b.c", "pw")
        self.client.force_login(self.admin)

    def test_list_only_project_actors(self):
        fake = FakeClient(actors=[actor("luna", "brood"), actor("mars", "sandbox")])
        with mock.patch.object(client_of, "_testing", fake):
            got = self.client.get("/api/projects/brood/actors")
        self.assertEqual(got.status_code, 200)
        self.assertEqual([a["name"] for a in got.json()["results"]], ["luna"])

    def test_list_state_and_retrieve(self):
        fake = FakeClient(
            actors=[actor("luna"), actor("parked", state=ateapi_pb2.ACTOR_STATE_SUSPENDED)])
        with mock.patch.object(client_of, "_testing", fake):
            listed = self.client.get("/api/projects/brood/actors").json()["results"]
            got = self.client.get("/api/projects/brood/actors/parked")
        self.assertEqual([a["state"] for a in listed], ["running", "suspended"])
        self.assertEqual(got.json()["name"], "parked")
        self.assertEqual(got.json()["state"], "suspended")

    def test_retrieve_missing(self):
        fake = FakeClient(actors=[], error=rpc_error("NOT_FOUND", "no such actor"))
        with mock.patch.object(client_of, "_testing", fake):
            got = self.client.get("/api/projects/brood/actors/luna")
        self.assertEqual(got.status_code, 404)
        self.assertEqual(got.json()["detail"], "no such actor")

    def test_suspend_and_resume(self):
        fake = FakeClient(actors=[actor("luna")])
        with mock.patch.object(client_of, "_testing", fake):
            suspended = self.client.post("/api/projects/brood/actors/luna",
                                         {"action": "suspend"})
            resumed = self.client.post("/api/projects/brood/actors/luna",
                                       {"action": "resume"})
        self.assertEqual(suspended.status_code, 200)
        self.assertEqual(suspended.json()["state"], "suspended")
        self.assertEqual(resumed.json()["state"], "running")
        self.assertEqual(fake.acted, [("suspend", "brood", "luna"),
                                      ("resume", "brood", "luna")])

    def test_unknown_action(self):
        fake = FakeClient(actors=[actor("luna")])
        with mock.patch.object(client_of, "_testing", fake):
            got = self.client.post("/api/projects/brood/actors/luna", {"action": "reboot"})
        self.assertEqual(got.status_code, 400)
        self.assertEqual(fake.acted, [])

    def test_delete(self):
        fake = FakeClient(actors=[actor("luna")])
        with mock.patch.object(client_of, "_testing", fake):
            got = self.client.delete("/api/projects/brood/actors/luna")
        self.assertEqual(got.status_code, 204)
        self.assertEqual(fake.acted, [("delete", "brood", "luna")])

    def test_anonymous_denied(self):
        self.client.logout()
        self.assertEqual(self.client.get("/api/projects/brood/actors").status_code, 403)


if __name__ == "__main__":  # pragma: no cover
    unittest.main()
