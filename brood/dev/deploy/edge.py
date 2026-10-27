"""Чистые манифесты публичного TLS-входа перед Go-сервером."""
import re


def manifests(image):
    """Вернуть новые объекты; проверка владения и сохранение Service — у вызывающего."""
    if not isinstance(image, str) or not re.fullmatch(
            r"localhost:5002/brood-go-edge@sha256:[0-9a-f]{64}", image):
        raise RuntimeError("Edge deployment needs an owned registry image pinned by sha256")

    def obj(kind, spec, api="v1"):
        return {"apiVersion": api, "kind": kind,
                "metadata": {"name": "edge", "namespace": "brood",
                             "labels": {"app.kubernetes.io/managed-by": "brood-go"}},
                "spec": spec}

    container = {
        "name": "edge", "image": image,
        "ports": [{"name": "http", "containerPort": 8080},
                  {"name": "https", "containerPort": 8443}],
        "securityContext": {"runAsNonRoot": True, "runAsUser": 65532, "runAsGroup": 65532,
                            "allowPrivilegeEscalation": False, "readOnlyRootFilesystem": True,
                            "capabilities": {"drop": ["ALL"]}},
        "volumeMounts": [{"name": "tls", "mountPath": "/etc/nginx/tls", "readOnly": True},
                         {"name": "tmp", "mountPath": "/tmp"}],
        "readinessProbe": {"tcpSocket": {"port": "https"}},
        "resources": {"requests": {"cpu": "25m", "memory": "32Mi"},
                      "limits": {"cpu": "1", "memory": "128Mi"}},
    }
    deployment = obj("Deployment", {
        "replicas": 1,
        # Selector существующего Deployment неизменяем: метка владельца только в template.
        "selector": {"matchLabels": {"app": "edge"}},
        "template": {
            "metadata": {"labels": {"app": "edge", "app.kubernetes.io/managed-by": "brood-go"}},
            "spec": {"automountServiceAccountToken": False, "enableServiceLinks": False,
                     "securityContext": {"fsGroup": 65532}, "containers": [container],
                     "volumes": [{"name": "tls", "secret": {"secretName": "brood-go-tls"}},
                                 {"name": "tmp", "emptyDir": {}}]}}}, "apps/v1")
    service = obj("Service", {
        "type": "NodePort", "selector": {"app": "edge"},
        "ports": [{"name": "http", "port": 80, "targetPort": 8080, "nodePort": 30080},
                  {"name": "https", "port": 443, "targetPort": 8443, "nodePort": 30443}]})
    policy = obj("NetworkPolicy", {
        "podSelector": {"matchLabels": {"app": "edge"}}, "policyTypes": ["Ingress", "Egress"],
        "ingress": [{"ports": [{"port": 8080, "protocol": "TCP"},
                                {"port": 8443, "protocol": "TCP"}]}],
        "egress": [
            {"to": [{"podSelector": {"matchLabels": {"app": "brood-go"}}}],
             "ports": [{"port": 8443, "protocol": "TCP"}]},
            {"to": [{"namespaceSelector": {"matchLabels": {
                "kubernetes.io/metadata.name": "kube-system"}},
                "podSelector": {"matchLabels": {"k8s-app": "kube-dns"}}}],
             "ports": [{"port": 53, "protocol": "UDP"}, {"port": 53, "protocol": "TCP"}]}]},
        "networking.k8s.io/v1")
    return [deployment, service, policy]
