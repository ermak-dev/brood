"""API проектов: разметка Atespace, перевод отказов grpc, права и CSRF."""
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


def _timestamp(iso):
    from google.protobuf.timestamp_pb2 import Timestamp

    t = Timestamp()
    t.FromJsonString(iso)
    return t


class FakeClient:
    """Ответы и отказы ateapi по списку имен -- чтобы не поднимать канал."""

    def __init__(self, atespaces=(), error=None):
        self.atespaces, self.error = list(atespaces), error
        self.created, self.deleted = [], []

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
        fake = FakeClient([atespace("brood")], rpc_error("NOT_FOUND", "no such atespace"))
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


if __name__ == "__main__":  # pragma: no cover
    unittest.main()
