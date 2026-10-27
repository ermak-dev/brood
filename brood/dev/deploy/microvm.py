"""Обратимое переключение пустой установки с gvisor на microVM."""
import copy
import hashlib
import json
import os
import re
import tempfile
import time

from brood.dev.deploy import infrastructure
from brood.dev.substrate import local


TARGET = "brood-microvm"
CONFIG = "microvm"
SOURCE_UID = "brood.dev/source-pool-uid"
ASSET_FILES = {"cloud-hypervisor": "cloud-hypervisor", "virtiofsd": "virtiofsd",
               "kata-kernel": "vmlinux", "kata-image": "rootfs.img"}


def source_pool(pool):
    """Исходный пул и предусловие Kubernetes нужны даже при нуле реплик."""
    metadata = pool.get("metadata", {}) if isinstance(pool, dict) else {}
    if not all(metadata.get(key) for key in ("uid", "resourceVersion", "name", "namespace")) \
            or metadata["name"] != "brood" or metadata["namespace"] != infrastructure.NAMESPACE \
            or not isinstance(pool.get("spec"), dict):
        raise RuntimeError("WorkerPool brood/brood needs its uid, resourceVersion and spec")
    if [c.get("name") for c in pool["spec"].get("sandboxClasses", [])] != ["gvisor"]:
        raise RuntimeError("WorkerPool brood/brood must be a gvisor pool")
    return pool


def worker_image(image):
    """Только microVM-воркер из реестра этой установки, закреплённый по digest."""
    if not isinstance(image, str) or not re.fullmatch(
            re.escape(infrastructure.REGISTRY) + r"/ateworker-microvm@sha256:[0-9a-f]{64}", image):
        raise RuntimeError("microVM worker image must be a digest in the installation registry")
    return image


def microvm_pool(pool, image):
    """Новый пул наследует spec, но не identity и владельцев исходного объекта."""
    source_pool(pool)
    worker_image(image)
    spec = copy.deepcopy(pool["spec"])
    spec.update(workerImage=image, sandboxClasses=[{
        "name": "microvm", "configRef": {"name": CONFIG}}])
    return {"apiVersion": "ate.dev/v1", "kind": "WorkerPool",
            "metadata": {"name": TARGET, "namespace": infrastructure.NAMESPACE,
                         "labels": {infrastructure.OWNER[0]: infrastructure.OWNER[1]},
                         "annotations": {SOURCE_UID: pool["metadata"]["uid"]}},
            "spec": spec}


def stopped_pool(pool):
    """Остановленный исходный пул сохраняет версию, образ, класс и настройки."""
    source_pool(pool)
    result = copy.deepcopy(pool)
    result.pop("status", None)
    result["spec"]["replicas"] = 0
    return result


def require_empty(response):
    """Любой актор требует отдельного решения о переносе, даже suspended."""
    if not isinstance(response, dict):
        raise RuntimeError("invalid actor list response")
    actors = response.get("actors", [])
    if not isinstance(actors, list) or any(
            not isinstance(actor, dict) or
            not isinstance(actor.get("metadata"), dict) or
            not actor["metadata"].get("name") for actor in actors):
        raise RuntimeError("invalid actor list response")
    if actors:
        names = ", ".join(actor["metadata"]["name"] for actor in actors)
        raise RuntimeError(f"refusing to stop the source pool while actors exist: {names}")


def require_drained(response, replicas):
    """Drain в ateapi закрывает размещение до последней проверки акторов."""
    workers = [worker for worker in response.get("workers", [])
               if worker.get("workerNamespace") == infrastructure.NAMESPACE
               and worker.get("workerPool") == "brood"]
    if len(workers) < replicas or any(worker.get("status", {}).get("state") !=
                                     "WORKER_STATE_DRAINING" for worker in workers):
        raise RuntimeError("drain every brood gvisor worker through the panel, then rerun "
                           "brood deploy microvm; the source pool has not been stopped")


def workers_ready(response, replicas):
    """Готовность Pod не доказывает регистрацию runtime в ateapi."""
    ready = 0
    for worker in response.get("workers", []):
        status = worker.get("status", {})
        if worker.get("workerNamespace") != infrastructure.NAMESPACE \
                or worker.get("workerPool") != TARGET \
                or status.get("state") != "WORKER_STATE_ACTIVE":
            continue
        if any(c.get("sandboxClass") == "microvm" and c.get("defaultRuntime", {}).get("version")
               for c in status.get("sandboxClasses", [])):
            ready += 1
    return ready >= replicas


class Deploy:
    """Новый runtime проверяется до остановки старого пула; удаления нет."""

    def __init__(self, cluster, clock=time.monotonic, sleep=time.sleep):
        self.cluster, self.shell = cluster, cluster.shell
        self.clock, self.sleep = clock, sleep
        self.deployment = infrastructure.Infrastructure(cluster)

    def migrate(self):
        self.cluster.check()
        pool, target = self._preflight()
        if target is None:
            image = self._prepare()
            target = microvm_pool(pool, image)
            self._write("create", target)
        else:
            image = worker_image(target["spec"]["workerImage"])
        self._wait(target["spec"]["replicas"])
        if pool["spec"]["replicas"]:
            require_drained(self._ate("workers"), pool["spec"]["replicas"])
            require_empty(self._actors())
            self._write("replace", stopped_pool(pool))
            self._wait_stopped()
        return image

    def _preflight(self):
        namespace = self._json("get", "namespace", infrastructure.NAMESPACE)
        if namespace.get("metadata", {}).get("labels", {}).get(infrastructure.OWNER[0]) != infrastructure.OWNER[1]:
            raise RuntimeError("namespace brood does not belong to brood")
        pool = source_pool(self._json("get", "workerpool", "brood", "-n", infrastructure.NAMESPACE))
        target = self._json("get", "workerpool", TARGET, "-n", infrastructure.NAMESPACE,
                            "--ignore-not-found")
        if target is not None:
            meta = target.get("metadata", {})
            if meta.get("annotations", {}).get(SOURCE_UID) != pool["metadata"]["uid"] \
                    or meta.get("labels", {}).get(infrastructure.OWNER[0]) != infrastructure.OWNER[1]:
                raise RuntimeError(f"WorkerPool {TARGET} does not belong to this migration")
            if target["spec"].get("sandboxClasses") != [
                    {"name": "microvm", "configRef": {"name": CONFIG}}]:
                raise RuntimeError(f"WorkerPool {TARGET} has a different sandbox configuration")
            worker_image(target["spec"].get("workerImage"))
        if (target or pool)["spec"].get("replicas", 0) < 1:
            raise RuntimeError("the microVM target needs at least one replica to verify its runtime")
        require_empty(self._actors())
        code, _, err = self.shell.output("docker", "exec", local.NODE, "test", "-c", "/dev/kvm")
        if code:
            raise RuntimeError(f"KVM is not available in {local.NODE}: {local.reason(err, code)}")
        nodes = self._json("get", "nodes")
        selector = pool["spec"].get("template", {}).get("nodeSelector", {})
        if not any(int(node.get("status", {}).get("allocatable", {}).get("ate.dev/kvm", "0")) > 0
                   and all(node["metadata"].get("labels", {}).get(k) == v
                           for k, v in selector.items()) for node in nodes.get("items", [])):
            raise RuntimeError("KVM is not available on a node matching the worker pool")
        return pool, target

    def _prepare(self):
        arch = self._output("go", "env", "GOARCH")
        env = dict(self.shell.env, NO_DEV_ENV="true", ATE_INSTALL_KIND="true",
                   BUCKET_NAME="ate-snapshots", KO_DEFAULTPLATFORMS=f"linux/{arch}")
        for key in ("ARCH", "OUT", "KATA_VER", "CH_VER"):
            env.pop(key, None)
        shell = local.Shell(env, self.cluster.source)
        template_path = os.path.join(self.cluster.source,
                                     "manifests/microvm/sandboxconfig-microvm.yaml.tmpl")
        with open(template_path, encoding="utf-8") as f:
            rendered = f.read().replace("${BUCKET_SCHEME}", "s3").replace(
                "${ASSET_BUCKET_NAME}", "ate-snapshots")
        with tempfile.TemporaryDirectory(prefix="brood-microvm.") as workdir:
            path = os.path.join(workdir, "config.yaml")
            with open(path, "w", encoding="utf-8") as f:
                f.write(rendered)
            desired = self._json("create", "--dry-run=client", "--validate=false", "-f", path)
        existing = self._json("get", "sandboxconfig", CONFIG, "--ignore-not-found")
        if existing is not None and existing.get("spec") != desired["spec"]:
            raise RuntimeError("microvm SandboxConfig already exists with different assets")
        shell.run(os.path.join(self.cluster.source, "hack/install-microvm-deps.sh"), "--install")
        assets = desired["spec"]["versions"][0]["assets"][arch]
        for name, filename in ASSET_FILES.items():
            path = os.path.join(self.cluster.source, "bin", "microvm-assets", arch, filename)
            with open(path, "rb") as f:
                digest = hashlib.file_digest(f, "sha256").hexdigest()
            if digest != assets[name]["sha256"]:
                raise RuntimeError(f"microVM asset {filename} checksum does not match SandboxConfig")
        return self.deployment._worker_image("microvm")

    def _actors(self):
        return self._ate("actors", "-A")

    def _ate(self, resource, *args):
        out = self._output(self.cluster.kubectl_ate, f"--context={local.CONTEXT}", "get",
                           resource, *args, "-o", "json")
        return self._parse(out, f"{resource} response")

    def _json(self, *args):
        out = self._output("kubectl", f"--context={local.CONTEXT}", *args,
                           "-o", "json", "--request-timeout=30s")
        return self._parse(out, "Kubernetes response") if out else None

    def _parse(self, out, what):
        try:
            result = json.loads(out)
            if not isinstance(result, dict):
                raise ValueError("expected an object")
            return result
        except ValueError as e:
            raise RuntimeError(f"invalid {what}: {e}") from None

    def _output(self, *args):
        code, out, err = self.shell.output(*args)
        if code:
            raise RuntimeError(f"{local.step(args)}: {local.reason(err, code)}")
        return out.strip()

    def _write(self, verb, manifest):
        with tempfile.TemporaryDirectory(prefix="brood-microvm.") as workdir:
            path = os.path.join(workdir, "pool.json")
            with open(path, "w", encoding="utf-8") as f:
                json.dump(manifest, f)
            self._output("kubectl", f"--context={local.CONTEXT}", verb, "-f", path,
                         "--request-timeout=30s")

    def _wait(self, replicas):
        self._output("kubectl", f"--context={local.CONTEXT}", "wait", "--for=create",
                     f"deployment/{TARGET}", "-n", infrastructure.NAMESPACE, "--timeout=600s")
        self._output("kubectl", f"--context={local.CONTEXT}", "rollout", "status",
                     f"deployment/{TARGET}", "-n", infrastructure.NAMESPACE, "--timeout=600s")
        deadline = self.clock() + 600
        while True:
            pool = self._json("get", "workerpool", TARGET, "-n", infrastructure.NAMESPACE)
            if pool.get("status", {}).get("readyReplicas", 0) >= replicas \
                    and workers_ready(self._ate("workers"), replicas):
                return
            if self.clock() >= deadline:
                raise RuntimeError("microVM runtime did not register within 600 seconds; "
                                   "the gvisor pool has not been stopped")
            self.sleep(2)

    def _wait_stopped(self):
        deadline = self.clock() + 300
        while True:
            pool = self._json("get", "workerpool", "brood", "-n", infrastructure.NAMESPACE)
            pods = self._json("get", "pods", "-n", infrastructure.NAMESPACE,
                              "-l", "ate.dev/worker-pool=brood")
            if pool.get("status", {}).get("replicas", 0) == 0 and not pods.get("items"):
                return
            if self.clock() >= deadline:
                raise RuntimeError("gvisor worker pods did not stop within 300 seconds")
            self.sleep(2)
