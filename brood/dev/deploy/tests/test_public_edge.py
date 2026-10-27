"""Публичный вход передаёт все маршруты Go без собственных ключей и панели."""
# FEATURE: Минимальный публичный TLS-вход для Go-сервера.
# SPEC: Закреплённый образ, прежний selector edge, NodePort, TLS-проверка,
# websocket/streaming, неизменённые Host/Origin, закрытый egress, без секретов в образе.
# IMPLEMENTATION: Чистый генератор трёх объектов и статический nginx без entrypoint-шаблонов.
# RESULT: IMPLEMENTED — 7 целевых тестов проходят; старый test_go_only запрещает каталог edge.
# STATUS: DONE — целевой контракт; обновление старого запрета остаётся основной интеграции.

import importlib
import importlib.util
from pathlib import Path
import unittest


ASSETS = Path(__file__).resolve().parents[1] / "edge"
IMAGE = "localhost:5002/brood-go-edge@sha256:" + "a" * 64


class PublicEdgeTests(unittest.TestCase):
    def render(self, image=IMAGE):
        self.assertIsNotNone(importlib.util.find_spec("brood.dev.deploy.edge"),
                             "pure edge manifest generator is missing")
        return importlib.import_module("brood.dev.deploy.edge").manifests(image)

    def asset(self, name):
        path = ASSETS / name
        self.assertTrue(path.is_file(), f"missing edge asset: {name}")
        return path.read_text()

    def test_manifest_contract_and_fresh_results(self):
        objects = self.render()
        self.assertIsInstance(objects, list)
        self.assertEqual([o["kind"] for o in objects], ["Deployment", "Service", "NetworkPolicy"])
        for obj in objects:
            self.assertEqual(obj["metadata"], {
                "name": "edge", "namespace": "brood",
                "labels": {"app.kubernetes.io/managed-by": "brood-go"}})
        objects[0]["metadata"]["labels"]["bad"] = "mutation"
        self.assertNotIn("bad", self.render()[0]["metadata"]["labels"])

    def test_only_owned_digest_images_are_accepted(self):
        for image in (None, "", 5, "localhost:5002/brood-go-edge:latest",
                      "elsewhere/brood-go-edge@sha256:" + "a" * 64,
                      "localhost:5002/brood-go@sha256:" + "a" * 64,
                      IMAGE + "\n", IMAGE[:-1], IMAGE.upper()):
            with self.subTest(image=image), self.assertRaisesRegex(RuntimeError, "owned registry image pinned by sha256"):
                self.render(image)

    def test_deployment_is_unprivileged_and_mounts_only_tls_and_tmp(self):
        deployment = self.render()[0]
        self.assertEqual(deployment["spec"]["selector"], {"matchLabels": {"app": "edge"}})
        template = deployment["spec"]["template"]
        self.assertEqual(template["metadata"]["labels"], {
            "app": "edge", "app.kubernetes.io/managed-by": "brood-go"})
        pod = template["spec"]
        self.assertFalse(pod["automountServiceAccountToken"])
        self.assertFalse(pod["enableServiceLinks"])
        self.assertEqual(pod["securityContext"]["fsGroup"], 65532)
        self.assertEqual(len(pod["containers"]), 1)
        container = pod["containers"][0]
        self.assertEqual(container["image"], IMAGE)
        self.assertEqual(container["securityContext"], {
            "runAsNonRoot": True, "runAsUser": 65532, "runAsGroup": 65532,
            "allowPrivilegeEscalation": False, "readOnlyRootFilesystem": True,
            "capabilities": {"drop": ["ALL"]}})
        self.assertNotIn("env", container)
        self.assertNotIn("envFrom", container)
        self.assertEqual(container["ports"], [
            {"name": "http", "containerPort": 8080}, {"name": "https", "containerPort": 8443}])
        self.assertEqual(pod["volumes"], [
            {"name": "tls", "secret": {"secretName": "brood-go-tls"}},
            {"name": "tmp", "emptyDir": {}}])
        self.assertEqual(container["volumeMounts"], [
            {"name": "tls", "mountPath": "/etc/nginx/tls", "readOnly": True},
            {"name": "tmp", "mountPath": "/tmp"}])
        self.assertEqual(container["readinessProbe"], {"tcpSocket": {"port": "https"}})

    def test_service_preserves_public_ports_without_claiming_server_metadata(self):
        service = self.render()[1]
        self.assertEqual(service["spec"], {
            "type": "NodePort", "selector": {"app": "edge"}, "ports": [
                {"name": "http", "port": 80, "targetPort": 8080, "nodePort": 30080},
                {"name": "https", "port": 443, "targetPort": 8443, "nodePort": 30443}]})
        self.assertNotIn("resourceVersion", service["metadata"])
        self.assertNotIn("uid", service["metadata"])

    def test_network_policy_has_only_public_ingress_go_and_dns_egress(self):
        policy = self.render()[2]
        self.assertEqual(policy["apiVersion"], "networking.k8s.io/v1")
        self.assertEqual(policy["spec"], {
            "podSelector": {"matchLabels": {"app": "edge"}},
            "policyTypes": ["Ingress", "Egress"],
            "ingress": [{"ports": [{"port": 8080, "protocol": "TCP"},
                                    {"port": 8443, "protocol": "TCP"}]}],
            "egress": [
                {"to": [{"podSelector": {"matchLabels": {"app": "brood-go"}}}],
                 "ports": [{"port": 8443, "protocol": "TCP"}]},
                {"to": [{"namespaceSelector": {"matchLabels": {
                    "kubernetes.io/metadata.name": "kube-system"}},
                    "podSelector": {"matchLabels": {"k8s-app": "kube-dns"}}}],
                 "ports": [{"port": 53, "protocol": "UDP"}, {"port": 53, "protocol": "TCP"}]}]})

    def test_nginx_redirects_http_and_routes_every_https_path_to_verified_go(self):
        conf = self.asset("nginx.conf")
        for directive in (
            "listen 8080;", "return 301 https://$http_host$request_uri;",
            "listen 8443 ssl;", "ssl_certificate /etc/nginx/tls/tls.crt;",
            "ssl_certificate_key /etc/nginx/tls/tls.key;", "access_log off;",
            "proxy_pass https://brood-go.brood.svc:8443;", "proxy_ssl_verify on;",
            "proxy_ssl_trusted_certificate /etc/nginx/tls/tls.crt;",
            "proxy_ssl_server_name on;", "proxy_ssl_name brood;",
            "proxy_set_header Host $http_host;", "proxy_set_header Origin $http_origin;",
            "proxy_set_header X-Forwarded-Proto https;", "proxy_http_version 1.1;",
            "proxy_set_header Upgrade $http_upgrade;",
            "proxy_set_header Connection $connection_upgrade;",
            "proxy_buffering off;", "proxy_request_buffering off;", "proxy_cache off;",
            "proxy_read_timeout 3600s;", "proxy_send_timeout 3600s;",
            "pid /tmp/nginx.pid;", "client_body_temp_path /tmp/client_temp;",
            "proxy_temp_path /tmp/proxy_temp;",
        ):
            with self.subTest(directive=directive):
                self.assertIn(directive, conf)
        self.assertEqual(conf.count("location / {"), 1)
        self.assertEqual(conf.count("proxy_pass "), 1)
        for forbidden in ("auth_request", "CLIPROXY", "django", "try_files", "root ", "access_log /", "proxy_set_header Authorization"):
            self.assertNotIn(forbidden, conf)

    def test_image_is_minimal_pinned_and_skips_mutating_entrypoint(self):
        dockerfile = self.asset("Dockerfile")
        self.assertIn("FROM nginxinc/nginx-unprivileged:1.30-alpine@sha256:"
                      "f5116ed4606f3eb015dd4a8e28cb9b351476dc0ad4bc6e9f93b19cec130cdf03", dockerfile)
        self.assertEqual(dockerfile.count("FROM "), 1)
        self.assertIn("COPY brood/dev/deploy/edge/nginx.conf /etc/nginx/nginx.conf", dockerfile)
        self.assertIn("USER 65532:65532", dockerfile)
        self.assertIn('ENTRYPOINT ["nginx"]', dockerfile)
        self.assertIn('CMD ["-g", "daemon off;"]', dockerfile)
        for forbidden in ("bun", "npm", "web/", "ARG ", "ENV ", "secrets/", ".env", "django", "CLIPROXY"):
            self.assertNotIn(forbidden, dockerfile)
        self.assertEqual(self.asset("Dockerfile.dockerignore").splitlines(), [
            "**", "!brood/dev/deploy/edge/nginx.conf"])
