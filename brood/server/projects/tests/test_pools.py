"""FEATURE: управление WorkerPool через ограниченный Kubernetes API.
SPEC: namespace brood, сессия и CSRF, uid/resourceVersion без обхода конфликтов,
без произвольного образа, ServiceAccount и полей pod; ошибки несут причину.
IMPLEMENTATION: REST-валидация и HTTPS-клиент с ротируемым токеном, без редиректов.
RESULT: IMPLEMENTED — права, версии, безопасные поля, TLS и отказы проверены.
STATUS: DONE
"""
import json
import tempfile
from pathlib import Path
from unittest import TestCase as UnitTestCase, mock

from django.contrib.auth.models import User
from django.test import TestCase, override_settings

IMAGE = "localhost:5002/worker@sha256:" + "a" * 64


def pool():
    return {
        "apiVersion": "ate.dev/v1", "kind": "WorkerPool",
        "metadata": {"name": "agents", "namespace": "brood", "uid": "u1",
                     "resourceVersion": "17", "creationTimestamp": "2026-10-25T00:00:00Z"},
        "spec": {"replicas": 1, "workerImage": IMAGE,
                 "sandboxClasses": [{"name": "gvisor"}],
                 "template": {"resources": {"requests": {"cpu": "250m", "memory": "2Gi"},
                                            "limits": {"cpu": "2", "memory": "2Gi"}}}},
        "status": {"replicas": 1, "readyReplicas": 1},
    }


@override_settings(WORKER_POOL_IMAGE=IMAGE)
class PoolApiTests(TestCase):
    def setUp(self):
        self.admin = User.objects.create_superuser("pool-admin", password="pass")
        self.client.force_login(self.admin)
        self.api = mock.Mock()
        self.patch = mock.patch("brood.server.projects.pools.client_of", return_value=self.api)
        self.patch.start()
        self.addCleanup(self.patch.stop)
        self.api.list.return_value = [pool()]
        self.api.get.return_value = pool()
        self.api.create.side_effect = lambda body: body
        self.api.update.side_effect = lambda name, body: body

    def test_list_detail_and_defaults(self):
        response = self.client.get("/api/worker-pools")
        self.assertEqual(response.status_code, 200)
        self.assertEqual(response.json(), {"results": [pool()]})
        self.assertEqual(self.client.get("/api/worker-pools/agents").json(), pool())
        self.assertEqual(self.client.get("/api/worker-pool-defaults").json(), {"workerImage": IMAGE})

    def test_create_forces_identity_and_removes_server_fields(self):
        body = pool()
        body["metadata"]["namespace"] = "other"
        response = self.client.post("/api/worker-pools", body, content_type="application/json")
        self.assertEqual(response.status_code, 201, response.content)
        sent = self.api.create.call_args.args[0]
        self.assertEqual(sent["metadata"], {"name": "agents", "namespace": "brood"})
        self.assertNotIn("status", sent)
        self.assertEqual(sent["spec"], body["spec"])

    def test_update_preserves_concurrency_and_url_identity(self):
        body = pool()
        body["spec"]["replicas"] = 0
        body["metadata"]["name"] = "other"
        response = self.client.put("/api/worker-pools/agents", body, content_type="application/json")
        self.assertEqual(response.status_code, 200, response.content)
        name, sent = self.api.update.call_args.args
        self.assertEqual(name, "agents")
        self.assertEqual(sent["metadata"], {"name": "agents", "namespace": "brood",
                                           "uid": "u1", "resourceVersion": "17"})
        self.assertEqual(sent["spec"]["replicas"], 0)
        self.api.get.assert_not_called()

    def test_missing_update_preconditions_and_invalid_json_are_refused(self):
        for body in ({"spec": pool()["spec"]}, [], None):
            response = self.client.put("/api/worker-pools/agents", json.dumps(body),
                                       content_type="application/json")
            self.assertEqual(response.status_code, 400, response.content)
        self.api.update.assert_not_called()

    def test_dangerous_pod_fields_and_unpinned_image_are_refused(self):
        for mutate in (
            lambda b: b["spec"].update(workerImage="evil@sha256:" + "b" * 64),
            lambda b: b["spec"]["template"].update(serviceAccountName="django"),
            lambda b: b["spec"]["template"].update(annotations={"evil": "value"}),
            lambda b: b["spec"].update(sandboxClasses=[{"name": "microvm"}]),
            lambda b: b["spec"].update(replicas=True),
            lambda b: b["spec"].update(replicas=-1),
            lambda b: b["spec"].update(replicas=1.5),
            lambda b: b["spec"].update(unrecognized="value"),
            lambda b: b["spec"].update(template=[]),
            lambda b: b["spec"]["template"]["resources"]["limits"].update({"example.com/gpu": "1"}),
            lambda b: b["spec"]["template"]["resources"]["limits"].update(memory={}),
            lambda b: b["metadata"].update(ownerReferences=[{"uid": "server"}]),
            lambda b: b["metadata"].update(finalizers=["evil.example/block"]),
        ):
            with self.subTest(mutate=mutate):
                body = pool()
                mutate(body)
                response = self.client.post("/api/worker-pools", body, content_type="application/json")
                self.assertEqual(response.status_code, 400, response.content)
        self.api.create.assert_not_called()

    def test_delete_requires_and_passes_both_preconditions(self):
        response = self.client.delete("/api/worker-pools/agents", {}, content_type="application/json")
        self.assertEqual(response.status_code, 400)
        response = self.client.delete("/api/worker-pools/agents",
                                      {"metadata": {"uid": "u1", "resourceVersion": "17"}},
                                      content_type="application/json")
        self.assertEqual(response.status_code, 204, response.content)
        self.api.delete.assert_called_once_with("agents", {"uid": "u1", "resourceVersion": "17"})

    def test_wrong_method_shapes_and_names(self):
        self.assertEqual(self.client.put("/api/worker-pools", {}, content_type="application/json").status_code, 405)
        self.assertEqual(self.client.post("/api/worker-pools/agents", {}, content_type="application/json").status_code, 405)
        self.assertEqual(self.client.delete("/api/worker-pools").status_code, 405)
        self.assertEqual(self.client.get("/api/worker-pools/..").status_code, 400)

    def test_conflict_keeps_kubernetes_reason(self):
        from brood.server.projects.kubernetes import KubeError
        self.api.update.side_effect = KubeError(409, "object has been modified")
        response = self.client.put("/api/worker-pools/agents", pool(), content_type="application/json")
        self.assertEqual(response.status_code, 409)
        self.assertIn("modified", response.json()["detail"])

    def test_dns_subdomain_names_match_the_panel(self):
        body = pool()
        body["metadata"]["name"] = "a" * 63 + ".agents"
        response = self.client.post("/api/worker-pools", body, content_type="application/json")
        self.assertEqual(response.status_code, 201, response.content)
        self.assertEqual(self.api.create.call_args.args[0]["metadata"]["name"], body["metadata"]["name"])

    @override_settings(KUBE_API_URL="https://api:invalid", KUBE_API_TOKEN="/token", KUBE_API_CA="/ca")
    def test_bad_endpoint_is_503_at_the_rest_boundary(self):
        self.patch.stop()
        response = self.client.get("/api/worker-pools")
        self.assertEqual(response.status_code, 503, response.content)
        self.assertIn("KUBE_API_URL", response.json()["detail"])

    def test_auth_and_csrf(self):
        self.client.logout()
        self.assertEqual(self.client.get("/api/worker-pools").status_code, 403)
        viewer = User.objects.create_user("viewer")
        self.client.force_login(viewer)
        self.assertEqual(self.client.get("/api/worker-pools").status_code, 403)
        from django.test import Client
        secure = Client(enforce_csrf_checks=True)
        secure.force_login(self.admin)
        self.assertEqual(secure.post("/api/worker-pools", pool(), content_type="application/json").status_code, 403)

    @override_settings(WORKER_POOL_IMAGE=None)
    def test_missing_image_is_explained(self):
        response = self.client.get("/api/worker-pool-defaults")
        self.assertEqual(response.status_code, 503)
        self.assertIn("WORKER_POOL_IMAGE", response.json()["detail"])


class KubeClientTests(UnitTestCase):
    def test_bad_endpoint_configuration_has_a_readable_refusal(self):
        from brood.server.projects.kubernetes import Client, KubeError
        for endpoint in ("http://api", "https://user:password@api", "https://api/path",
                         "https://api:invalid", "https://api:99999", "https://[broken"):
            with self.subTest(endpoint=endpoint):
                with self.assertRaises(KubeError) as caught:
                    Client(endpoint, "/token", "/ca")
                self.assertEqual(caught.exception.status, 503)
                self.assertIn("KUBE_API_URL", str(caught.exception))

    def test_tls_timeout_token_rotation_pagination_and_delete_preconditions(self):
        from brood.server.projects.kubernetes import Client
        with tempfile.TemporaryDirectory() as tmp:
            token = Path(tmp) / "token"
            token.write_text("first\n")
            connection = mock.Mock()
            connection.getresponse.side_effect = [
                mock.Mock(status=200, read=lambda: json.dumps({"items": [pool()], "metadata": {"continue": "a/b+"}}).encode()),
                mock.Mock(status=200, read=lambda: b'{"items": [], "metadata": {}}'),
                mock.Mock(status=200, read=lambda: b'{}'),
            ]
            with mock.patch("brood.server.projects.kubernetes.ssl.create_default_context") as tls, \
                 mock.patch("brood.server.projects.kubernetes.http.client.HTTPSConnection", return_value=connection) as connect:
                api = Client("https://kubernetes.default.svc", str(token), "/trusted/ca")
                self.assertEqual(api.list(), [pool()])
                token.write_text("second\n")
                api.delete("agents", {"uid": "u1", "resourceVersion": "17"})
                tls.assert_called_with(cafile="/trusted/ca")
                self.assertEqual(connect.call_args.kwargs["timeout"], 10)
                calls = connection.request.call_args_list
                self.assertEqual(calls[0].args[:2], ("GET", "/apis/ate.dev/v1/namespaces/brood/workerpools?limit=500"))
                self.assertIn("continue=a%2Fb%2B", calls[1].args[1])
                self.assertEqual(calls[0].kwargs["headers"]["Authorization"], "Bearer first")
                self.assertEqual(calls[2].kwargs["headers"]["Authorization"], "Bearer second")
                self.assertEqual(json.loads(calls[2].kwargs["body"])["preconditions"], {"uid": "u1", "resourceVersion": "17"})
                self.assertEqual(connection.close.call_count, 3)

    def test_missing_credentials_are_a_service_refusal(self):
        from brood.server.projects.kubernetes import Client, KubeError
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaises(KubeError) as caught:
                Client("https://kubernetes.default.svc", str(Path(tmp) / "missing"), "/ca").get("agents")
            self.assertEqual(caught.exception.status, 503)
            self.assertIn("KUBE_API_TOKEN", str(caught.exception))

    def test_http_error_timeout_and_non_json_are_safe(self):
        from brood.server.projects.kubernetes import Client, KubeError
        with tempfile.TemporaryDirectory() as tmp:
            token = Path(tmp) / "token"
            token.write_text("secret-token")
            connection = mock.Mock()
            with mock.patch("brood.server.projects.kubernetes.ssl.create_default_context"), \
                 mock.patch("brood.server.projects.kubernetes.http.client.HTTPSConnection", return_value=connection):
                api = Client("https://kubernetes.default.svc", str(token), "/ca")
                for status, payload, expected in ((409, b'{"message":"stale version"}', 409),
                                                   (500, b'<html>broken</html>', 502),
                                                   (302, b'', 502)):
                    connection.getresponse.return_value = mock.Mock(status=status, read=lambda: payload)
                    with self.assertRaises(KubeError) as caught:
                        api.get("agents")
                    self.assertEqual(caught.exception.status, expected)
                    self.assertNotIn("secret-token", str(caught.exception))
                connection.request.side_effect = TimeoutError("timed out")
                with self.assertRaises(KubeError) as caught:
                    api.get("agents")
                self.assertEqual(caught.exception.status, 503)
