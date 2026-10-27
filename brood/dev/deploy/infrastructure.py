"""Общие шаги инфраструктуры установки, без серверного runtime."""
import ipaddress
import json
import os
import re
import ssl

from brood.common import config
from brood.dev.substrate import local

HERE = os.path.dirname(os.path.realpath(__file__))
MANIFESTS = os.path.join(HERE, "manifests")
NAMESPACE = "brood"
OWNER = ("app.kubernetes.io/managed-by", "brood")
REGISTRY = f"localhost:{local.REGISTRY_PORT}"
TLS = {"tls.crt": "secrets/tls.crt", "tls.key": "secrets/tls.key"}


def env_file(values):
    """Разобранные настройки -> однострочный env-файл без shell-кавычек."""
    for key, value in values.items():
        if "\n" in value:
            raise RuntimeError(f"{key} in .env spans lines; a Secret value must be one line")
    return "".join(f"{key}={value}\n" for key, value in values.items())


def initial_pool(image):
    """Шаблон начального gvisor-пула; не часть серверной выкладки."""
    return {
        "apiVersion": "ate.dev/v1", "kind": "WorkerPool",
        "metadata": {"name": "brood", "namespace": NAMESPACE},
        "spec": {"replicas": 1, "workerImage": image,
                 "sandboxClasses": [{"name": "gvisor"}],
                 "template": {"resources": {
                     "requests": {"cpu": "250m", "memory": "2Gi"},
                     "limits": {"cpu": "2", "memory": "2Gi"}}}}}


def api_egress(service, endpoints):
    """Точные адреса API до и после DNAT; ошибка discovery не расширяет доступ."""
    def rule(addresses, ports):
        if not addresses or not ports:
            raise ValueError("no ready addresses or HTTPS ports")
        cidrs = sorted({str(ipaddress.ip_network(ipaddress.ip_address(ip))) for ip in addresses})
        if any(type(port) is not int or not 0 < port < 65536 for port in ports):
            raise ValueError("invalid HTTPS port")
        return {"to": [{"ipBlock": {"cidr": cidr}} for cidr in cidrs],
                "ports": [{"protocol": "TCP", "port": port} for port in sorted(set(ports))]}

    try:
        spec = service["spec"]
        https = next(p for p in spec["ports"]
                     if p["port"] == 443 and p.get("protocol", "TCP") == "TCP")
        ips = spec.get("clusterIPs") or [spec["clusterIP"]]
        rules = [rule(ips, [https["port"]])]
        subsets = endpoints["subsets"]
        if not subsets:
            raise ValueError("no endpoint subsets")
        for subset in subsets:
            ports = [p["port"] for p in subset["ports"]
                     if p.get("name", "") == https.get("name", "")
                     and p.get("protocol", "TCP") == "TCP"]
            rules.append(rule([a["ip"] for a in subset["addresses"]], ports))
        return rules
    except (AttributeError, KeyError, TypeError, ValueError, StopIteration) as e:
        raise RuntimeError(f"Kubernetes API discovery: unusable default/kubernetes Service "
                           f"or Endpoints: {e or 'no HTTPS service port'}") from None


class Infrastructure:
    """TLS, проверка владения, discovery и сборка microVM-воркера."""

    def __init__(self, cluster, project=config.PROJECT):
        self.cluster, self.shell, self.project = cluster, cluster.shell, project

    def _tls(self):
        paths = {name: os.path.join(self.project, rel) for name, rel in TLS.items()}
        missing = [TLS[name] for name, path in paths.items() if not os.path.isfile(path)]
        if missing:
            raise RuntimeError(f"Go deployment needs the TLS certificate and key: "
                               f"no {', '.join(missing)} in {self.project}")
        try:
            ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER).load_cert_chain(paths["tls.crt"], paths["tls.key"])
        except ssl.SSLError as e:
            raise RuntimeError(f"{TLS['tls.key']} does not match {TLS['tls.crt']}: "
                               f"{e.reason or e}") from None
        out = {}
        for name, path in paths.items():
            with open(path, encoding="utf-8") as f:
                out[name] = f.read()
        return out

    def _namespace_is_ours(self):
        key, owner = OWNER
        code, out, err = self.shell.output(
            "kubectl", f"--context={local.CONTEXT}", "get", "namespace", NAMESPACE,
            "--ignore-not-found", "-o", "json", "--request-timeout=15s")
        if code:
            raise RuntimeError(f"cannot inspect namespace {NAMESPACE}: {local.reason(err, code)}")
        if not out.strip():
            raise RuntimeError("Go deployment needs the existing owned namespace brood and trusted ServiceAccount django")
        try:
            labels = json.loads(out)["metadata"].get("labels", {})
            if not isinstance(labels, dict):
                raise ValueError("labels are not an object")
        except (ValueError, AttributeError, KeyError, TypeError):
            raise RuntimeError(f"cannot inspect namespace {NAMESPACE}: invalid JSON") from None
        if labels.get(key) != owner:
            raise RuntimeError(f"namespace {NAMESPACE} exists without label {key}={owner}: "
                               f"it is not brood's; refusing to deploy into it")

    def _api_egress(self):
        objects = []
        for resource in ("service", "endpoints"):
            code, out, err = self.shell.output(
                "kubectl", f"--context={local.CONTEXT}", "get", resource, "kubernetes",
                "-n", "default", "-o", "json", "--request-timeout=15s")
            if code:
                raise RuntimeError(f"Kubernetes API discovery: get {resource} default/kubernetes: "
                                   f"{local.reason(err, code)}")
            try:
                objects.append(json.loads(out))
            except ValueError:
                raise RuntimeError(f"Kubernetes API discovery: invalid JSON for {resource}") from None
        return api_egress(*objects)

    def _worker_image(self, sandbox_class="gvisor"):
        """Pinned ko, кэши, version stamp и host platform из checkout Substrate."""
        if sandbox_class not in ("gvisor", "microvm"):
            raise RuntimeError(f"unsupported worker sandbox class: {sandbox_class}")
        worker = f"ateworker-{sandbox_class}"
        env = dict(self.shell.env, KO_DOCKER_REPO=REGISTRY)
        env.setdefault("KO_CACHE", os.path.join(local.state_dir(self.cluster.source), "ko-cache"))
        shell = local.Shell(env, self.cluster.source)

        def output(*args):
            code, out, err = shell.output(*args)
            if code:
                raise RuntimeError(f"worker image {local.step(args)}: {local.reason(err, code)}")
            return out.strip()

        env["KO_DEFAULTPLATFORMS"] = "linux/" + output("go", "env", "GOARCH")
        version = env.get("VERSION")
        if not version:
            code, out, _ = shell.output("git", "describe", "--tags", "--always", "--dirty")
            version = out.strip() if code == 0 and out.strip() else "dev"
        out = output(os.path.join(self.cluster.source, "hack", "run-tool.sh"), "ko", "build",
                     f"go.ate.dev/substrate/cmd/{worker}", "--base-import-paths",
                     f"--ldflags=-X=go.ate.dev/substrate/internal/version.Version={version}")
        image = out.split()[-1] if out else ""
        if not re.fullmatch(re.escape(REGISTRY) + f"/{worker}@sha256:[0-9a-f]{{64}}", image):
            raise RuntimeError(f"worker image ko build returned no registry digest for {worker}")
        return image
