"""Чистые манифесты изолированного окружения разработки Brood."""

import copy
import ipaddress
from pathlib import Path
import re

from brood.common import config
import yaml


def _declarations():
    # Это только чтение шаблонов; проверка схемы и компиляция принадлежат Go.
    return yaml.safe_load(Path(config.PROJECT).joinpath("Broodfile").read_text())


PUBLIC_HOSTS = tuple(_declarations()["networks"]["development"]["tls"]["hosts"])
CLAUDE_HOSTS = tuple(host for host in _declarations()["networks"]["coding"]["tls"]["hosts"]
                     if host not in PUBLIC_HOSTS)
_DEFAULT_NAME = object()


def _dns_label(value):
    if not isinstance(value, str) or not re.fullmatch(
            r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?", value):
        raise ValueError("name must be a lowercase DNS label of 1 to 63 characters")


def _image(value):
    component = r"[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*"
    registry = r"[a-z0-9]+(?:[.-][a-z0-9]+)*(?::[0-9]+)?"
    reference = (rf"(?:{registry}/)?{component}(?:/{component})*"
                 r"(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?@sha256:[0-9a-f]{64}")
    if not isinstance(value, str) or len(value) > 512 or not re.fullmatch(reference, value):
        raise ValueError("image must be a valid OCI reference pinned by sha256 digest")


def _hostname(value):
    if not isinstance(value, str) or len(value) > 253 or "." not in value:
        raise ValueError("egress hostname must be an exact public DNS hostname")
    try:
        ipaddress.ip_address(value)
    except ValueError:
        pass
    else:
        raise ValueError("egress hostname must not be an IP address")
    if value.endswith((".local", ".localhost", ".internal", ".invalid", ".test")):
        raise ValueError("egress hostname must not be internal")
    try:
        for label in value.split("."):
            _dns_label(label)
    except ValueError:
        raise ValueError("egress hostname must be an exact public DNS hostname") from None


def _profile(value):
    if value not in ("build", "claude-code"):
        raise ValueError("workbench profile must be build or claude-code")
    return value


def profile_of(template):
    values = [env.get("value") for container in template.get("containers", [])
              for env in container.get("env", []) if env.get("name") == "BROOD_WORKBENCH_PROFILE"]
    if len(values) > 1:
        raise ValueError("template has ambiguous workbench profile markers")
    return _profile(values[0] if values else "build")


def actor_template(image, atespace, name=_DEFAULT_NAME, profile="build"):
    """Шаблон API: запуск задания только явно, без автоматического golden actor."""
    _profile(profile)
    if name is _DEFAULT_NAME:
        name = "brood-claude-code" if profile == "claude-code" else "brood-build-test-deploy"
    _image(image)
    _dns_label(atespace)
    _dns_label(name)
    target = "coding" if profile == "claude-code" else "development"
    result = copy.deepcopy(_declarations()["templates"][target]["actor"])
    result["metadata"] = {"atespace": atespace, "name": name}
    result["containers"][0]["image"] = image
    return result


def worker_pool(worker_image):
    """Отдельный microVM-пул с меткой для селектора шаблона разработки."""
    _image(worker_image)
    return {
        "apiVersion": "ate.dev/v1", "kind": "WorkerPool",
        "metadata": {"name": "brood-development", "namespace": "brood",
                     "labels": {"brood.dev/purpose": "development"}},
        "spec": {
            "replicas": 1, "workerImage": worker_image,
            "sandboxClasses": [{"name": "microvm", "configRef": {"name": "microvm"}}],
            "template": {
                "labels": {"brood.dev/purpose": "development"},
                "nodeSelector": {"kubernetes.io/arch": "amd64"},
                "resources": {"requests": {"cpu": "2", "memory": "8Gi"},
                              "limits": {"cpu": "6", "memory": "10Gi"}}},
        },
    }


def egress_policy(atespace, actor, hosts=PUBLIC_HOSTS, profile="build"):
    """Политика API; родительский actor передаётся отдельно при создании."""
    _profile(profile)
    _dns_label(atespace)
    _dns_label(actor)
    if not isinstance(hosts, (tuple, list)) or not 1 <= len(hosts) <= 256:
        raise ValueError("egress hosts must contain 1 to 256 exact public hostnames")
    hosts = tuple(hosts) + (CLAUDE_HOSTS if profile == "claude-code" else ())
    if len(hosts) > 256:
        raise ValueError("egress hosts must contain at most 256 exact public hostnames")
    for host in hosts:
        _hostname(host)
    target = "coding" if profile == "claude-code" else "development"
    network = _declarations()["networks"][target]
    return {
        "metadata": {"atespace": atespace, "name": "default"},
        "rules": [
            {"tlsPassthrough": {"hostnames": list(dict.fromkeys(hosts)),
                                "ports": {"numbers": list(network["tls"]["ports"])}}},
            {"http": {"hostnames": list(network["http"]["hosts"]),
                      "ports": {"numbers": list(network["http"]["ports"])}}},
        ],
    }
