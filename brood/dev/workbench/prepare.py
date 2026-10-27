"""Инфраструктура приватного Go endpoint только в изолированном госте."""
import json
import os
from pathlib import Path
import tempfile

import yaml

from brood.dev.deploy.infrastructure import HERE, Infrastructure, env_file
from brood.dev.substrate import local
from brood.dev.workbench import runtime


OWNER = {"app.kubernetes.io/managed-by": "brood", "brood.dev/purpose": "workbench"}


def _objects():
    manifests = Path(HERE) / "manifests"
    objects = list(yaml.safe_load_all((manifests / "namespace.yaml").read_text()))
    objects += json.loads((manifests / "worker-access.json").read_text())["items"]
    for obj in objects:
        obj["metadata"].setdefault("labels", {}).update(OWNER)
    return objects


def _inspect(cluster, kind, name, namespace=None):
    args = ["kubectl", f"--context={local.CONTEXT}", "get", kind, name,
            "--ignore-not-found", "-o", "json", "--request-timeout=15s"]
    if namespace:
        args.extend(["-n", namespace])
    code, out, _ = cluster.shell.output(*args)
    if code:
        raise RuntimeError(f"cannot inspect guest {kind}/{name}")
    if not out.strip():
        return False
    try:
        labels = json.loads(out)["metadata"].get("labels", {})
        if not isinstance(labels, dict):
            raise ValueError("invalid labels")
    except (ValueError, KeyError, TypeError, AttributeError):
        raise RuntimeError(f"cannot inspect guest {kind}/{name}: invalid metadata") from None
    if any(labels.get(key) != value for key, value in OWNER.items()):
        raise RuntimeError(f"refusing foreign guest {kind}/{name}")
    return True


def prepare(cluster, root, env):
    """Ни public ingress, ни пользователи установки здесь не создаются."""
    runtime.check_environment(env)
    root = Path(root).resolve()
    if root == Path("/") or Path(cluster.source).resolve() != root / "substrate":
        raise RuntimeError("workbench preparation requires the guest Substrate checkout")
    settings = root / "brood/.env"
    if settings.is_symlink() or not settings.is_file():
        raise RuntimeError("workbench preparation requires private guest settings")
    cluster.check()
    exists = _inspect(cluster, "namespace", "brood")
    objects = _objects()
    missing = objects
    if exists:
        missing = [obj for obj in objects[1:]
                   if not _inspect(cluster, obj["kind"], obj["metadata"]["name"], "brood")]
    infrastructure = Infrastructure(cluster, project=str(root / "brood"))
    images = {"gvisor": infrastructure._worker_image("gvisor")}
    with tempfile.TemporaryDirectory(prefix="brood-guest-prepare-") as directory:
        path = Path(directory) / "infrastructure.json"
        for obj in missing:
            path.write_text(json.dumps(obj))
            code, _, _ = cluster.shell.output("kubectl", f"--context={local.CONTEXT}",
                                               "create", "-f", str(path))
            if code:
                raise RuntimeError(f"cannot create guest {obj['kind']}/{obj['metadata']['name']}; "
                                   "Go endpoint was not deployed")
    # Append сохраняет исходный session secret; config читает последнее значение.
    fd = os.open(settings, os.O_WRONLY | os.O_APPEND | os.O_NOFOLLOW)
    with os.fdopen(fd, "a", encoding="utf-8") as output:
        os.fchmod(output.fileno(), 0o600)
        output.write("\n" + env_file({"WORKER_POOL_IMAGES": json.dumps(images, sort_keys=True)}))
    return images
