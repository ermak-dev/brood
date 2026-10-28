"""Go-сервер на отдельном PVC, без автоматического публичного cutover."""

import ipaddress
import json
from pathlib import Path
import re
import tempfile
import uuid

from brood.common import config
from brood.dev.deploy.infrastructure import Infrastructure
from brood.dev.substrate import local


OWNER = {"app.kubernetes.io/managed-by": "brood-go"}


def approved_images(raw):
    try:
        images = json.loads(raw)
    except (ValueError, TypeError):
        raise RuntimeError("WORKER_POOL_IMAGES must contain deployed digest-pinned images") from None
    if (not isinstance(images, dict) or "gvisor" not in images or set(images) - {"gvisor", "microvm", "lxc"}
            or any(not isinstance(value, str) or not re.fullmatch(r"[a-z0-9][a-z0-9._:/-]*@sha256:[0-9a-f]{64}", value)
                   for value in images.values())):
        raise RuntimeError("WORKER_POOL_IMAGES must contain deployed digest-pinned images")
    return images


def registry_endpoint(raw):
    try:
        address = ipaddress.IPv4Address(json.loads(raw)["IPAddress"])
        if not address.is_private or address.is_loopback or address.is_unspecified or address.is_multicast:
            raise ValueError
    except (ValueError, KeyError, TypeError):
        raise RuntimeError("cannot discover the owned internal registry endpoint") from None
    return {"url": f"http://{address}:5000", "ip": str(address)}


def manifests(image, worker_images=None, proxy=False, registry=None):
    if not isinstance(image, str) or not re.fullmatch(r"localhost:5002/brood-go@sha256:[0-9a-f]{64}", image):
        raise RuntimeError("Go deployment needs an owned registry image pinned by sha256")
    workers = approved_images(json.dumps(worker_images))
    labels = {"app": "brood-go", **OWNER}

    def obj(kind, name, spec, api="v1"):
        return {"apiVersion": api, "kind": kind,
                "metadata": {"name": name, "namespace": "brood", "labels": OWNER.copy()}, "spec": spec}

    projected = {"sources": [
        {"serviceAccountToken": {"audience": "api.ate-system.svc", "expirationSeconds": 3600, "path": "token"}},
        {"clusterTrustBundle": {"signerName": "servicedns.podcert.ate.dev/identity",
                               "labelSelector": {"matchLabels": {"podcert.ate.dev/canarying": "live"}},
                               "path": "trust-bundle.pem"}},
        {"podCertificate": {"signerName": "podidentity.podcert.ate.dev/identity", "keyType": "ECDSAP256",
                            "credentialBundlePath": "client.pem"}}]}
    env = {"WORKER_POOL_IMAGES": json.dumps(workers, sort_keys=True), "BROOD_DB_PATH": "/data/brood.db", "BROOD_ADDR": ":8443",
           "BROOD_TLS_CERT": "/var/run/brood-tls/tls.crt", "BROOD_TLS_KEY": "/var/run/brood-tls/tls.key",
           "BROOD_SECURE_COOKIES": "true", "ATEAPI_TOKEN_FILE": "/var/run/ateapi/token",
           "ATEAPI_CA_FILE": "/var/run/ateapi/trust-bundle.pem",
           "ATENET_ROUTER": "atenet-router.ate-system.svc:443",
           "ATENET_CLIENT_CERT_FILE": "/var/run/ateapi/client.pem",
           "ATENET_CLIENT_KEY_FILE": "/var/run/ateapi/client.pem",
           "ATENET_CA_FILE": "/var/run/ateapi/trust-bundle.pem",
           "ATENET_SERVER_NAME": "atenet-router.ate-system.svc",
           "KUBE_API_URL": "https://kubernetes.default.svc",
           "KUBE_API_TOKEN": "/var/run/secrets/kubernetes.io/serviceaccount/token",
           "KUBE_API_CA": "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"}
    if registry:
        env.update(BROOD_REGISTRY_URL=registry["url"], BROOD_REGISTRY_IMAGE_HOST=f"localhost:{local.REGISTRY_PORT}")
    container = {"name": "brood", "image": image, "args": ["serve"],
                 "ports": [{"name": "https", "containerPort": 8443}],
                 "securityContext": {"runAsNonRoot": True, "runAsUser": 65532,
                                     "allowPrivilegeEscalation": False, "readOnlyRootFilesystem": True,
                                     "capabilities": {"drop": ["ALL"]}},
                 "env": [{"name": k, "value": v} for k, v in env.items()] + [
                     {"name": "BROOD_SESSION_SECRET", "valueFrom": {"secretKeyRef": {
                         "name": "brood-go-env", "key": "BROOD_SESSION_SECRET"}}}],
                 "volumeMounts": [{"name": name, "mountPath": path, "readOnly": readonly}
                                  for name, path, readonly in (("data", "/data", False),
                                      ("ateapi", "/var/run/ateapi", True),
                                      ("tls", "/var/run/brood-tls", True), ("tmp", "/tmp", False))],
                 "readinessProbe": {"httpGet": {"path": "/healthz", "port": "https", "scheme": "HTTPS"}},
                 "resources": {"requests": {"cpu": "100m", "memory": "128Mi"},
                               "limits": {"cpu": "2", "memory": "512Mi"}}}
    if proxy:
        container["env"].extend({"name": key, "valueFrom": {"secretKeyRef": {
            "name": "brood-go-env", "key": key}}} for key in ("CLIPROXY_URL", "CLIPROXY_MANAGEMENT_KEY"))
    deployment = obj("Deployment", "brood-go", {
        "replicas": 1, "strategy": {"type": "Recreate"}, "selector": {"matchLabels": {"app": "brood-go"}},
        "template": {"metadata": {"labels": labels}, "spec": {
            # Историческое имя — точный доверенный принципал в Substrate,
            # не Django runtime. Переименование требует миграции доверия.
            "serviceAccountName": "django", "enableServiceLinks": False,
            "securityContext": {"fsGroup": 65532}, "containers": [container],
            "volumes": [{"name": "data", "persistentVolumeClaim": {"claimName": "brood-go-data"}},
                        {"name": "ateapi", "projected": projected},
                        {"name": "tls", "secret": {"secretName": "brood-go-tls"}},
                        {"name": "tmp", "emptyDir": {}}]}}}, "apps/v1")
    objects = [obj("PersistentVolumeClaim", "brood-go-data", {
                "accessModes": ["ReadWriteOnce"], "resources": {"requests": {"storage": "1Gi"}}}),
            obj("Service", "brood-go", {"selector": {"app": "brood-go"},
                "ports": [{"name": "https", "port": 8443, "targetPort": "https"}]}), deployment,
            obj("NetworkPolicy", "brood-go", {
                "podSelector": {"matchLabels": {"app": "brood-go"}}, "policyTypes": ["Ingress", "Egress"],
                "ingress": [{"from": [{"podSelector": {"matchLabels": {"app": "edge"}}}],
                             "ports": [{"port": 8443}]}],
                "egress": [
                    {"to": [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "kube-system"}},
                             "podSelector": {"matchLabels": {"k8s-app": "kube-dns"}}}],
                     "ports": [{"port": 53, "protocol": "UDP"}, {"port": 53, "protocol": "TCP"}]},
                    {"to": [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "ate-system"}}}],
                     "ports": [{"port": 443}]},
                    # Service:443 маршрутизатора перенаправляется на pod:8443 до egress-проверки.
                    {"to": [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "ate-system"}},
                             "podSelector": {"matchLabels": {"app": "atenet-router"}}}],
                     "ports": [{"port": 8443, "protocol": "TCP"}]}]}, "networking.k8s.io/v1")]
    if proxy:
        objects[-1]["spec"]["egress"].append({"to": [{"podSelector": {"matchLabels": {"app": "cliproxy"}}}],
                                              "ports": [{"port": 8317}]})
    if registry:
        objects[-1]["spec"]["egress"].append({"to": [{"ipBlock": {"cidr": registry["ip"] + "/32"}}],
                                              "ports": [{"port": 5000, "protocol": "TCP"}]})
    access = json.loads((Path(__file__).parent / "manifests" / "worker-access.json").read_text())
    source_role = next(item for item in access["items"] if item["kind"] == "Role")
    pod_rules = [rule for rule in source_role["rules"]
                 if rule["apiGroups"] == [""] and rule["resources"] == ["pods"]]
    if not pod_rules:
        raise RuntimeError("worker pod diagnostic permissions are not configured")
    name = "brood-go-pod-diagnostics"
    metadata = {"name": name, "namespace": "brood", "labels": OWNER.copy()}
    role = {"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role",
            "metadata": metadata.copy(), "rules": pod_rules}
    binding = {"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
               "metadata": metadata.copy(),
               "subjects": [{"kind": "ServiceAccount", "name": "django", "namespace": "brood"}],
               "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": name}}
    return [role, binding, *objects]


class Deploy:
    def __init__(self, cluster, get=config.get, project=config.PROJECT):
        self.cluster, self.get, self.project = cluster, get, Path(project)

    def _registry_access(self):
        code, out, _ = self.cluster.shell.output("docker", "inspect", "--format",
            "{{json .NetworkSettings.Networks.kind}}", local.REGISTRY)
        if code:
            raise RuntimeError("cannot inspect the owned internal registry")
        return registry_endpoint(out)

    def _worker_images(self):
        raw = self.get("WORKER_POOL_IMAGES")
        if raw:
            return approved_images(raw)
        fallback = self.get("WORKER_POOL_IMAGE")
        if fallback:
            return approved_images(json.dumps({"gvisor": fallback}))

        code, out, _ = self.cluster.shell.output("kubectl", f"--context={local.CONTEXT}",
            "-n", "brood", "get", "deployment", "brood-go", "-o", "json", "--request-timeout=15s")
        if code:
            raise RuntimeError("Go deployment needs WORKER_POOL_IMAGES in .env or an existing owned Go deployment")
        try:
            deployment = json.loads(out)
            if not all(deployment["metadata"].get("labels", {}).get(k) == v for k, v in OWNER.items()):
                raise RuntimeError("refusing foreign deployment/brood-go")
            container = next(c for c in deployment["spec"]["template"]["spec"]["containers"] if c["name"] == "brood")
            raw = next(e["value"] for e in container["env"] if e["name"] == "WORKER_POOL_IMAGES")
        except (AttributeError, KeyError, StopIteration, TypeError, ValueError):
            raise RuntimeError("cannot read configured WORKER_POOL_IMAGES from Go deployment") from None
        return approved_images(raw)

    def run(self):
        secret = self.get("BROOD_SESSION_SECRET")
        if not secret or len(secret) < 32:
            raise RuntimeError("Go deployment needs BROOD_SESSION_SECRET of at least 32 characters in .env")
        environment = {"BROOD_SESSION_SECRET": secret}
        proxy_url = self.get("CLIPROXY_URL")
        if proxy_url:
            proxy_key = self.get("CLIPROXY_MANAGEMENT_KEY")
            if not proxy_key:
                raise RuntimeError("CLIPROXY_MANAGEMENT_KEY is required with CLIPROXY_URL")
            environment.update(CLIPROXY_URL=proxy_url, CLIPROXY_MANAGEMENT_KEY=proxy_key)
        infrastructure = Infrastructure(self.cluster, project=str(self.project))
        tls = infrastructure._tls()
        self.cluster.check()
        infrastructure._namespace_is_ours()
        workers = self._worker_images()
        for kind, name in (("deployment", "brood-go"), ("service", "brood-go"),
                           ("pvc", "brood-go-data"), ("secret", "brood-go-env"), ("secret", "brood-go-tls"),
                           ("role", "brood-go-pod-diagnostics"), ("rolebinding", "brood-go-pod-diagnostics"),
                           ("networkpolicy", "brood-go"), ("networkpolicy", "brood-go-cliproxy")):
            code, out, _ = self.cluster.shell.output("kubectl", f"--context={local.CONTEXT}",
                "get", kind, name, "-n", "brood", "--ignore-not-found", "-o", "json")
            if code:
                raise RuntimeError(f"cannot check ownership of {kind}/{name}")
            if out.strip():
                try:
                    labels = json.loads(out)["metadata"].get("labels", {})
                    owned = all(labels.get(k) == v for k, v in OWNER.items())
                except (AttributeError, KeyError, TypeError, ValueError):
                    raise RuntimeError(f"cannot check ownership of {kind}/{name}: invalid JSON") from None
                if not owned:
                    raise RuntimeError(f"refusing foreign {kind}/{name}")
        api_rules = infrastructure._api_egress()
        registry = self._registry_access()
        with tempfile.TemporaryDirectory(prefix="brood-go-deploy-") as directory:
            root = Path(directory)
            metadata = root / "metadata.json"
            tag = "localhost:5002/brood-go:build-" + uuid.uuid4().hex
            release = self.get("BROOD_RELEASE_SHA")
            if release and not re.fullmatch(r"[0-9a-f]{40}", release):
                raise RuntimeError("BROOD_RELEASE_SHA must be a full commit sha")
            version_args = ("--build-arg", f"VERSION={release}") if release else ()
            code, out, err = self.cluster.shell.output("docker", "buildx", "build", "--push", "--platform",
                "linux/amd64", *version_args, "--metadata-file", str(metadata), "-t", tag,
                "-f", str(self.project / "deploy/Dockerfile.go"), str(self.project))
            if code:
                raise RuntimeError("Go image build failed: " + (err or out)[-4000:])
            digest = json.loads(metadata.read_text()).get("containerimage.digest", "")
            if not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
                raise RuntimeError("Go image build returned invalid digest metadata")
            image = "localhost:5002/brood-go@" + digest
            objects = manifests(image, workers, proxy=bool(proxy_url), registry=registry)
            objects[-1]["spec"]["egress"].extend(api_rules)
            if proxy_url:
                objects.append({"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
                    "metadata": {"name": "brood-go-cliproxy", "namespace": "brood", "labels": OWNER.copy()},
                    "spec": {"podSelector": {"matchLabels": {"app": "cliproxy"}}, "policyTypes": ["Ingress"],
                             "ingress": [{"from": [{"podSelector": {"matchLabels": {"app": "brood-go"}}}],
                                          "ports": [{"port": 8317}]}]}})
            for name, values, secret_type in (("brood-go-env", environment, "Opaque"),
                                             ("brood-go-tls", tls, "kubernetes.io/tls")):
                objects.append({"apiVersion": "v1", "kind": "Secret", "type": secret_type,
                                "metadata": {"name": name, "namespace": "brood", "labels": OWNER.copy()},
                                "stringData": values})
            path = root / "release.json"
            path.touch(mode=0o600)
            path.write_text(json.dumps({"apiVersion": "v1", "kind": "List", "items": objects}))
            code, _, _ = self.cluster.shell.output("kubectl", f"--context={local.CONTEXT}", "apply", "-f", str(path))
            if code:
                raise RuntimeError("Go deployment apply failed; public ingress was not changed")
            code, _, err = self.cluster.shell.output("kubectl", f"--context={local.CONTEXT}", "-n", "brood",
                                                   "rollout", "status", "deployment/brood-go", "--timeout=300s")
            if code:
                raise RuntimeError("Go deployment rollout failed: " + err[-2000:])
            return image
