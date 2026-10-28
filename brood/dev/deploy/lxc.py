"""Явная подготовка доверенного LXC runtime без миграции пулов и акторов."""
import json
import os
import re
import tempfile
import uuid

from brood.dev.deploy import infrastructure
from brood.dev.substrate import local


CONFIG = "lxc-image"
DEFAULT_ANNOTATION = "sandboxconfig.ate.dev/is-class-default"


def worker_image(value):
    """Точный LXC-репозиторий установки и неизменяемый digest."""
    if not isinstance(value, str) or not re.fullmatch(
            re.escape(infrastructure.REGISTRY) + r"/ateworker-lxc@sha256:[0-9a-f]{64}", value):
        raise RuntimeError("LXC worker image must be a digest in the installation registry")
    return value


def image_digest(raw):
    """RepoDigests собранного образа -> единственный digest этой установки."""
    try:
        values = json.loads(raw)
        if not isinstance(values, list):
            raise ValueError
        matching = [value for value in values if isinstance(value, str) and re.fullmatch(
            re.escape(infrastructure.REGISTRY) + r"/ateworker-lxc@sha256:[0-9a-f]{64}", value)]
        if len(matching) != 1:
            raise ValueError
        return matching[0]
    except (TypeError, ValueError):
        raise RuntimeError("LXC worker image push returned no unique installation digest") from None


def require_api(crd):
    """Старый или неизвестный CRD не разрешает менять конфигурацию runtime."""
    try:
        version = next(version for version in crd["spec"]["versions"]
                       if version["name"] == "v1" and version["served"] is True)
        schema = version["schema"]["openAPIV3Schema"]["properties"]["spec"]["properties"]
        if "lxc" not in schema["sandboxClasses"]["items"]["properties"]["name"]["enum"]:
            raise ValueError
    except (AttributeError, KeyError, TypeError, ValueError, StopIteration):
        raise RuntimeError("installed Substrate does not advertise LXC; install the verified fork first") from None


def configuration(response):
    """Собственный явный config; глобальный class-default не назначается."""
    desired = {"apiVersion": "ate.dev/v1", "kind": "SandboxConfig", "metadata": {
        "name": CONFIG, "labels": {infrastructure.OWNER[0]: infrastructure.OWNER[1]}},
        "spec": {"sandboxClass": "lxc", "defaultVersion": "image", "versions": [
            {"name": "image", "assets": {"amd64": {}, "arm64": {}}}]}}
    try:
        items = response["items"]
        if not isinstance(items, list):
            raise ValueError
        existing = None
        for item in items:
            metadata = item["metadata"]
            if metadata["name"] == CONFIG:
                if existing is not None:
                    raise ValueError
                existing = item
        if existing is None:
            return desired
        metadata = existing["metadata"]
        if (metadata.get("labels", {}).get(infrastructure.OWNER[0]) != infrastructure.OWNER[1]
                or metadata.get("annotations", {}).get(DEFAULT_ANNOTATION) not in (None, "false")
                or not metadata.get("uid") or not metadata.get("resourceVersion")
                or metadata.get("deletionTimestamp") or existing["spec"] != desired["spec"]):
            raise RuntimeError("LXC SandboxConfig exists without matching ownership, identity or spec")
        return None
    except (AttributeError, KeyError, TypeError, ValueError):
        raise RuntimeError("cannot inspect LXC SandboxConfigs: invalid inventory") from None


def output(shell, *args):
    """Шаг с захваченным выводом, без трассировки и вывода библиотекой."""
    code, out, err = shell.output(*args)
    if code:
        raise RuntimeError(f"LXC {local.step(args)}: {local.reason(err, code)}")
    return out.strip()


def build_image(cluster):
    """Dockerfile форка содержит LXC userspace; ko/distroless здесь непригоден."""
    directory = os.path.join(cluster.source, "cmd", "ateworker-lxc")
    if any(not os.path.isfile(os.path.join(directory, name))
           for name in ("build.sh", "Dockerfile", "sandboxconfig.yaml")):
        raise RuntimeError("Substrate source has no verified LXC worker build files")
    shell = local.Shell(dict(cluster.shell.env), cluster.source)
    if output(shell, "go", "env", "GOARCH") not in ("amd64", "arm64"):
        raise RuntimeError("LXC worker build supports amd64 and arm64 only")
    tag = f"{infrastructure.REGISTRY}/ateworker-lxc:brood-{uuid.uuid4().hex}"
    if output(shell, "docker", "image", "ls", "--filter", f"reference={tag}",
              "--format", "{{.Repository}}:{{.Tag}}"):
        raise RuntimeError("temporary LXC image tag already exists; refusing to overwrite it")
    shell.run(os.path.join(directory, "build.sh"), tag)
    identity = output(shell, "docker", "image", "inspect", "--format", "{{.Id}}", tag)
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", identity):
        raise RuntimeError("cannot confirm the temporary LXC image identity")
    cluster.check()
    shell.run("docker", "push", tag)
    image = image_digest(output(shell, "docker", "image", "inspect", "--format", "{{json .RepoDigests}}", tag))
    if output(shell, "docker", "image", "inspect", "--format", "{{.Id}}", tag) != identity:
        raise RuntimeError("temporary LXC image tag changed; refusing cleanup")
    shell.run("docker", "image", "rm", tag)
    return image


class Deploy:
    """Готовит только образ и явный config, не создаёт и не останавливает пулы."""

    def __init__(self, cluster, build=None):
        self.cluster, self.shell = cluster, cluster.shell
        self.build = build or build_image

    def prepare(self):
        self.cluster.check()
        infrastructure.Infrastructure(self.cluster)._namespace_is_ours()
        require_api(self._json("get", "crd", "workerpools.ate.dev"))
        desired = configuration(self._json("get", "sandboxconfigs"))
        image = worker_image(self.build(self.cluster))
        if desired is not None:
            self.cluster.check()
            with tempfile.TemporaryDirectory(prefix="brood-lxc.") as directory:
                path = os.path.join(directory, "config.json")
                with open(path, "w", encoding="utf-8") as stream:
                    json.dump(desired, stream)
                output(self.shell, "kubectl", f"--context={local.CONTEXT}", "create", "-f", path,
                       "--request-timeout=30s")
        return image

    def _json(self, *args):
        raw = output(self.shell, "kubectl", f"--context={local.CONTEXT}", *args,
                     "-o", "json", "--request-timeout=30s")
        try:
            value = json.loads(raw)
            if not isinstance(value, dict):
                raise ValueError
            return value
        except (TypeError, ValueError):
            raise RuntimeError("cannot inspect LXC configuration: invalid JSON") from None
