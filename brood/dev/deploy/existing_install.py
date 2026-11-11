"""Закрытые snapshot и schema gate существующей Go-БД, без bootstrap и авторизации."""
import copy
import hashlib
import datetime
import fcntl
import json
import math
import os
from pathlib import Path
import re
import socket
import tempfile
import time
import uuid

from brood.dev.deploy.go_server import OWNER
from brood.dev.substrate import local


class RetentionRefused(RuntimeError):
    """Отказ самого helper prune (#247): pod завершился Failed или ответ нечестен.
    Только он - предупреждение после прошедшей выкладки; сбой жизненного цикла helper
    (identity, удаление по UID, API) - отказ выкладки, как у inspect."""


class ExistingInstall:
    def __init__(self, deployment, clock=time.monotonic, sleep=time.sleep, wall=time.time, fenced=None,
                 lock_path=None):
        self.deployment = deployment
        # Scale-to-zero отключён, пока безопасность запоздавшего patch не проверена на живом
        # кластере: включает только явный opt-in оператора в .env (docs/DEPLOY.md).
        get = getattr(deployment, "get", None)
        self.fenced = fenced if fenced is not None else (callable(get) and get("BROOD_DEPLOY_SCALE_FENCE") == "live-verified")
        self.clock, self.sleep, self.wall = clock, sleep, wall
        # Восстановление помним отдельно от state: rebase после rollout заменяет state.
        self.recovered = False
        self.state = None
        self.before = None
        self.snapshot_path = None
        # Запись нашего scale-to-zero: uid, id записи и spec после подтверждённого чтением scale.
        self.quiesced = None
        # Аренда параллельного запуска: идентичность процесса и состояние нашей Lease.
        self.holder = uuid.uuid4().hex
        self.lease = None
        # Потеря владения Lease - липкое терминальное состояние: после неё ни одной записи.
        self.evicted = False
        self.beaten = 0.0
        self.started = wall()
        # Process lock (flock) машины запуска: ядро снимает его вместе с процессом. Он не
        # заменяет Lease кластера, а дополняет её: Lease видят все, flock - только эта машина.
        self.lock_path = lock_path or str(Path(local.managed_checkout(os.environ)).parent / "deploy.lock")
        self.lock_fd = None
        # После возврата before_apply apply мог уйти в кластер: исход любой ошибки неоднозначен.
        self.handed_off = False
        # Неоднозначный исход settle внутри recover: записи в Deployment могли пройти.
        self.ambiguous = False

    def owned(self, kind, name):
        obj = self.deployment.get_object(kind, name)
        meta = (obj or {}).get("metadata", {})
        if (meta.get("name") != name or meta.get("namespace") != "brood" or not meta.get("uid")
                or any(meta.get("labels", {}).get(k) != v for k, v in OWNER.items())):
            raise RuntimeError(f"existing deployment requires owned {kind}/{name}")
        return obj

    def check(self, recoverable=False):
        """recoverable=True - только для read-only --check: запись оборванного deploy,
        которую full run вправе разобрать, не отказ, а статус 'recoverable'."""
        try:
            return self._check(recoverable)
        except (KeyError, TypeError, StopIteration, AttributeError, ValueError):
            raise RuntimeError("existing deployment has unsupported runtime or database configuration") from None

    def _check(self, recoverable=False):
        self.refuse_foreign_lease()
        deployment = self.owned("deployment", "brood-go")
        service = self.owned("service", "brood-go")
        pvc = self.owned("pvc", "brood-go-data")
        if self.NOTE in deployment["metadata"].get("annotations", {}):
            if recoverable:
                return self.assess(deployment)
            raise RuntimeError("deployment/brood-go carries a quiesce record from an interrupted deploy; "
                               "run 'bin/brood deploy server' to settle it or follow the manual recovery "
                               "procedure in docs/DEPLOY.md")
        if (deployment["spec"].get("replicas") != 1
                or deployment["spec"]["selector"]["matchLabels"] != {"app": "brood-go"}
                or service["spec"].get("selector") != {"app": "brood-go"}
                or pvc.get("status", {}).get("phase") != "Bound"):
            raise RuntimeError("existing deployment requires ready owned runtime and bound volume")
        volume = self.deployment.get_object("pv", pvc["spec"]["volumeName"])
        claim = (volume or {}).get("spec", {}).get("claimRef", {})
        if (not volume or any(claim.get(k) != v for k, v in {
                "name": "brood-go-data", "namespace": "brood", "uid": pvc["metadata"]["uid"]}.items())):
            raise RuntimeError("existing database volume identity does not match owned PVC")
        template = deployment["spec"]["template"]["spec"]
        path = self.database_path(template)
        pods = self.deployment.get_object("pods", "-l=app=brood-go")["items"]
        active = [p for p in pods if not p["metadata"].get("deletionTimestamp")
                  and p.get("status", {}).get("phase") not in {"Succeeded", "Failed"}]
        if len(active) != 1:
            raise RuntimeError("existing deployment needs exactly one ready current runtime")
        pod = active[0]
        meta, status = pod["metadata"], pod.get("status", {})
        owner = next(o for o in meta["ownerReferences"] if o.get("controller"))
        rs = self.deployment.get_object("replicaset", owner["name"])
        rs_owner = next(o for o in rs["metadata"]["ownerReferences"] if o.get("controller"))
        current = deployment.get("status", {})
        image = template["containers"][0]["image"]
        digest = re.fullmatch(r"localhost:5002/brood-go@(sha256:[0-9a-f]{64})", image)
        runtime = next(c for c in status.get("containerStatuses", []) if c.get("name") == "brood")
        if (not digest
                or not runtime.get("imageID", "").endswith("@" + digest[1])
                or owner.get("kind") != "ReplicaSet" or owner.get("uid") != rs["metadata"].get("uid")
                or rs_owner.get("kind") != "Deployment" or rs_owner.get("uid") != deployment["metadata"]["uid"]
                or meta.get("namespace") != "brood" or not meta.get("uid")
                or any(meta.get("labels", {}).get(k) != v for k, v in OWNER.items())
                or current.get("observedGeneration", 0) < deployment["metadata"].get("generation", 1)
                or self.database_path(pod["spec"]) != path
                or pod["spec"]["containers"][0]["image"] != template["containers"][0]["image"]
                or not pod["spec"].get("nodeName")):
            raise RuntimeError("existing deployment runtime identity or readiness mismatch")
        state = {"path": path, "node": pod["spec"]["nodeName"], "volume": volume["metadata"]["uid"],
                 "identities": {"deployment": deployment["metadata"]["uid"],
                                "service": service["metadata"]["uid"], "pvc": pvc["metadata"]["uid"]}}
        ready = (runtime.get("ready") and status.get("phase") == "Running"
                 and any(c.get("type") == "Ready" and c.get("status") == "True" for c in status.get("conditions", []))
                 and current.get("updatedReplicas") == 1 and current.get("availableReplicas") == 1)
        if not ready:
            # Единственное исключение: прерванный rollout, доказанный целиком (#220).
            if not self.fenced:
                raise RuntimeError("crash-loop recovery needs scale-to-zero, which is blocked: safety against a "
                                   "late patch is not verified on a live cluster (docs/DEPLOY.md); "
                                   "nothing was changed")
            state["mode"] = "recovery"
            state["recovery"] = self.crashloop(deployment, rs, pod, runtime, len(pods))
        return state

    def crashloop(self, deployment, rs, pod, runtime, listed):
        """Доказывает, что не-готовый единственный pod - это CrashLoopBackOff текущей
        ревизии Deployment из-за несовместимой схемы. Любая неопределённость - отказ.
        Причину по одной строке лога не считаем доказательством безопасности: это
        лишь необходимое условие, совместимость схемы с новым образом проверяет
        отдельный verify-helper по snapshot до apply."""
        refusal = RuntimeError("existing deployment runtime identity or readiness mismatch")
        current, status = deployment.get("status", {}), pod.get("status", {})
        revision = "deployment.kubernetes.io/revision"
        wanted = deployment["metadata"].get("annotations", {}).get(revision)
        restarts = runtime.get("restartCount")
        terminated = runtime.get("lastState", {}).get("terminated", {})
        if (listed != 1 or status.get("phase") != "Running" or runtime.get("ready") is not False
                or any(c.get("type") == "Ready" and c.get("status") == "True" for c in status.get("conditions", []))
                or runtime.get("state", {}).get("waiting", {}).get("reason") != "CrashLoopBackOff"
                or type(restarts) is not int or restarts < 1
                or terminated.get("exitCode") != 1 or terminated.get("reason") != "Error"
                or not wanted or rs["metadata"].get("annotations", {}).get(revision) != wanted
                or current.get("updatedReplicas") != 1 or current.get("replicas") != 1
                or current.get("availableReplicas", 0) != 0 or current.get("readyReplicas", 0) != 0):
            raise refusal
        # Ни одна другая ревизия не держит pod: иначе это обычный незавершённый rollout.
        for other in self.deployment.get_object("replicaset", "-l=app=brood-go")["items"]:
            if (other["metadata"].get("uid") != rs["metadata"]["uid"]
                    and (other.get("spec", {}).get("replicas") or other.get("status", {}).get("replicas"))):
                raise refusal
        code, out, _ = self.deployment.cluster.shell.output(
            "kubectl", f"--context={local.CONTEXT}", "-n", "brood", "logs", pod["metadata"]["name"],
            "-c", "brood", "--previous", "--tail=20", "--limit-bytes=16384", "--request-timeout=15s")
        if code or not re.search(r"incompatible database schema: (table \w+ differs from current schema"
                                 r"|missing table \w+|found \d+, want \d+)", out):
            raise RuntimeError("existing deployment crash is not a proven database schema incompatibility")
        # generation здесь не доказательство: apiserver увеличивает его и при смене аннотаций,
        # поэтому неизменность Deployment доказывается digest всего spec.
        return {"pod": pod["metadata"]["uid"], "replicaset": rs["metadata"]["uid"],
                "spec": self.spec_digest(deployment["spec"], 0),
                "image": deployment["spec"]["template"]["spec"]["containers"][0]["image"]}

    @staticmethod
    def database_path(spec):
        # Rollout пишет /data/brood.db: иной layout нельзя молча переключать на пустую БД.
        containers = spec["containers"]
        if len(containers) != 1 or spec.get("initContainers"):
            raise RuntimeError("unsupported existing database configuration")
        container = containers[0]
        entries = [e for e in container.get("env", []) if e["name"] == "BROOD_DB_PATH"]
        mounts = [{"readOnly": False, **m} for m in container["volumeMounts"] if m["mountPath"] == "/data"]
        volumes = [v for v in spec["volumes"] if v["name"] == "data"]
        if (container.get("name") != "brood" or container.get("args") != ["serve"]
                or len(entries) != 1 or entries[0].get("value") != "/data/brood.db"
                or mounts != [{"name": "data", "mountPath": "/data", "readOnly": False}]
                or volumes != [{"name": "data", "persistentVolumeClaim": {"claimName": "brood-go-data"}}]
                or any(m["mountPath"].startswith("/data/") for m in container["volumeMounts"])):
            raise RuntimeError("unsupported existing database path or volume configuration")
        return entries[0]["value"]

    def recheck(self):
        state = self.check()
        if self.state is not None and state != self.state:
            raise RuntimeError("existing deployment identity changed before database helper")
        self.state = state
        return state

    def rebase(self):
        """После восстановления pod новый и готовый; владение ресурсами и данными прежнее."""
        state = self.check()
        stable = ("path", "volume", "identities")
        if state.get("mode") or any(state[k] != self.state[k] for k in stable):
            raise RuntimeError("existing deployment identity changed after recovery rollout")
        self.state = state

    def manifests(self, image, state, operation, name, destination, sha256=None, keep=None):
        if not re.fullmatch(r"localhost:5002/brood-go@sha256:[0-9a-f]{64}", image):
            raise RuntimeError("database helper requires the newly built digest-pinned Go image")
        if operation not in {"snapshot", "inspect", "verify", "prune"}:
            raise RuntimeError("unsupported database helper operation")
        labels = {**OWNER, "brood.dev/database-helper": name}
        metadata = {"name": name, "namespace": "brood", "labels": labels}
        args = ["--path", state["path"]]
        if operation in {"snapshot", "verify", "prune"}:
            if not destination or not re.fullmatch(r"/data/\.brood-snapshot-[a-z0-9-]+\.db", destination):
                raise RuntimeError("invalid private database snapshot destination")
        if operation == "snapshot":
            args += ["--output", destination]
        if operation == "verify":
            if not isinstance(sha256, str) or not re.fullmatch(r"[0-9a-f]{64}", sha256):
                raise RuntimeError("verify helper requires the snapshot sha256 hash")
            # Проверяется verified snapshot, а не живая БД: источник подменён.
            args = ["--path", destination, "--sha256", sha256]
        if operation == "prune":
            # Хранение снимков (#247): снимок этой выкладки helper не удаляет никогда.
            if type(keep) is not int or keep < 1:
                raise RuntimeError("snapshot retention must keep at least one snapshot")
            args = ["--current", destination, "--keep", str(keep)]
        pod = {"apiVersion": "v1", "kind": "Pod", "metadata": metadata, "spec": {
            "restartPolicy": "Never", "activeDeadlineSeconds": 180, "terminationGracePeriodSeconds": 10,
            "automountServiceAccountToken": False, "enableServiceLinks": False,
            "affinity": {"nodeAffinity": {"requiredDuringSchedulingIgnoredDuringExecution": {"nodeSelectorTerms": [
                {"matchFields": [{"key": "metadata.name", "operator": "In", "values": [state["node"]]}]}]}}},
            "securityContext": {"runAsNonRoot": True, "runAsUser": 65532, "runAsGroup": 65532},
            "containers": [{"name": "database", "image": image, "command": ["/brood", "database", operation],
                "args": args, "securityContext": {"allowPrivilegeEscalation": False, "privileged": False,
                    "runAsNonRoot": True, "runAsUser": 65532, "runAsGroup": 65532,
                    "seccompProfile": {"type": "RuntimeDefault"},
                    "readOnlyRootFilesystem": True, "capabilities": {"drop": ["ALL"]}},
                "resources": {"requests": {"cpu": "100m", "memory": "64Mi"},
                              "limits": {"cpu": "1", "memory": "256Mi"}},
                "volumeMounts": [{"name": "data", "mountPath": "/data",
                                  "readOnly": operation not in {"snapshot", "prune"}}]}],
            "volumes": [{"name": "data", "persistentVolumeClaim": {"claimName": "brood-go-data"}}]}}
        policy = {"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy", "metadata": metadata,
                  "spec": {"podSelector": {"matchLabels": {"brood.dev/database-helper": name}},
                           "policyTypes": ["Ingress", "Egress"], "ingress": [], "egress": []}}
        return pod, policy

    @staticmethod
    def metadata(raw, snapshot=False):
        try:
            data = json.loads(raw)
            keys = {"schema_version", "enabled_admins", "users"} | ({"sha256", "bytes"} if snapshot else set())
            if not isinstance(data, dict) or set(data) != keys:
                raise ValueError
            if any(type(data[k]) is not int or data[k] < 1 for k in ("schema_version", "enabled_admins", "users")):
                raise ValueError
            if data["enabled_admins"] > data["users"]:
                raise ValueError
            if snapshot and (type(data["bytes"]) is not int or data["bytes"] <= 0
                             or not isinstance(data["sha256"], str) or not re.fullmatch(r"[0-9a-f]{64}", data["sha256"])):
                raise ValueError
            return data
        except (ValueError, TypeError, KeyError):
            raise RuntimeError("database helper returned invalid metadata or no enabled administrator") from None

    def retention(self, raw, keep):
        """Ответ prune: сохранён именно снимок этой выкладки, не больше keep снимков."""
        try:
            data = json.loads(raw)
            if (not isinstance(data, dict) or set(data) != {"current", "kept", "removed"}
                    or data["current"] != Path(self.snapshot_path).name
                    or type(data["kept"]) is not int or not 1 <= data["kept"] <= keep
                    or type(data["removed"]) is not int or data["removed"] < 0):
                raise ValueError
            return data
        except (ValueError, TypeError):
            raise RetentionRefused("snapshot retention helper returned an invalid result or did not keep "
                                   "this deploy's snapshot") from None

    @staticmethod
    def verification(raw):
        try:
            data = json.loads(raw)
            if (not isinstance(data, dict) or set(data) != {"schema_version", "verified"}
                    or type(data["schema_version"]) is not int or data["schema_version"] < 1
                    or data["verified"] is not True):
                raise ValueError
            return data
        except (ValueError, TypeError):
            raise RuntimeError("schema verification helper returned invalid or negative result") from None

    def kubectl(self, *args):
        return self.deployment.cluster.shell.output(
            "kubectl", f"--context={local.CONTEXT}", "-n", "brood", *args, "--request-timeout=15s")

    def command(self, *args):
        self.beat()
        code, out, _ = self.kubectl(*args)
        if code:
            raise RuntimeError("owned database helper failed; no automatic database restore is attempted")
        return out

    @staticmethod
    def validate_identity(actual, expected, uid):
        meta = (actual or {}).get("metadata", {})
        if (not uid or meta.get("uid") != uid
                or any(meta.get(k) != expected["metadata"][k] for k in ("name", "namespace"))
                or any(meta.get("labels", {}).get(k) != v for k, v in expected["metadata"]["labels"].items())):
            raise RuntimeError("database helper identity changed; refusing access or cleanup")

    def validate(self, actual, expected, uid):
        self.validate_identity(actual, expected, uid)
        spec = copy.deepcopy(actual.get("spec", {}))
        # Только известные omitempty-defaults: writable mount и пустые правила deny-all.
        if expected["kind"] == "NetworkPolicy":
            for key in ("ingress", "egress"):
                spec.setdefault(key, [])
        elif expected["kind"] == "Pod":
            for container in spec.get("containers", []):
                for mount in container.get("volumeMounts", []):
                    mount.setdefault("readOnly", False)
        # API defaults допустимы, дополнительные credentials, mounts и sidecars — нет.
        def contains(current, wanted):
            if isinstance(wanted, dict):
                return isinstance(current, dict) and all(k in current and contains(current[k], v) for k, v in wanted.items())
            if isinstance(wanted, list):
                return isinstance(current, list) and len(current) == len(wanted) and all(contains(a, b) for a, b in zip(current, wanted))
            return current == wanted
        if not contains(spec, expected["spec"]):
            raise RuntimeError("database helper scheduling, volume or security configuration changed")
        if expected["kind"] == "Pod" and (spec.get("hostNetwork") or spec.get("hostPID") or spec.get("hostIPC")
                or spec.get("initContainers") or spec.get("ephemeralContainers")
                or any(c.get("env") or c.get("envFrom")
                       or c.get("securityContext", {}).get("capabilities", {}).get("add") for c in spec["containers"])
                or (spec.get("nodeName") and spec["nodeName"] != self.state["node"])):
            raise RuntimeError("unsafe database helper runtime configuration")

    def execute(self, image, operation, keep=None):
        self.require_owner()
        state = self.guard()
        name = "brood-db-" + uuid.uuid4().hex
        if operation == "snapshot":
            self.snapshot_path = "/data/.brood-snapshot-" + uuid.uuid4().hex + ".db"
        sha256 = self.before["sha256"] if operation == "verify" and self.before else None
        pod, policy = self.manifests(image, state, operation, name, self.snapshot_path, sha256=sha256, keep=keep)
        created = []
        with tempfile.TemporaryDirectory(prefix="brood-database-helper-") as directory:
            path = Path(directory) / "helper.json"
            path.touch(mode=0o600)
            try:
                for obj in (policy, pod):
                    path.write_text(json.dumps(obj))
                    raw = self.command("create", "-f", str(path), "-o", "json")
                    try:
                        actual = json.loads(raw)
                        uid = actual["metadata"]["uid"]
                    except (ValueError, KeyError, TypeError):
                        raise RuntimeError("cannot identify created database helper; refusing rollout") from None
                    created.append((obj, uid))
                    self.validate(actual, obj, uid)
                self.await_helper(operation, name, pod, created[-1][1])
                if self.quiesced:
                    # Пока helper держал PVC, никто другой его не монтировал.
                    self.exclusive_volume(allow={created[-1][1]})
                raw = self.command("logs", name, "-c", "database", "--limit-bytes=4096")
                if operation == "verify":
                    return self.verification(raw)
                if operation == "prune":
                    return self.retention(raw, keep)
                return self.metadata(raw, snapshot=operation == "snapshot")
            finally:
                for obj, uid in reversed(created):
                    kind = obj["kind"].lower()
                    current = self.deployment.get_object(kind, name)
                    # Отклонённый spec тоже удаляем, но только при совпадении записанного UID.
                    self.validate_identity(current, obj, uid)
                    api = "/api/v1" if kind == "pod" else "/apis/networking.k8s.io/v1"
                    path.write_text(json.dumps({"apiVersion": "v1", "kind": "DeleteOptions",
                        "preconditions": {"uid": uid}, "propagationPolicy": "Foreground"}))
                    plural = "pods" if kind == "pod" else "networkpolicies"
                    self.command("delete", "--raw", f"{api}/namespaces/brood/{plural}/{name}", "-f", str(path))
                    if kind == "pod":
                        self.command("wait", "--for=delete", "pod/" + name, "--timeout=30s")

    # Закрытый словарь причин: ничего из pod или лога не попадает в отказ иначе.
    REASONS = {"Error", "OOMKilled", "DeadlineExceeded", "Evicted", "ContainerCannotRun", "StartError",
               "ImagePullBackOff", "ErrImagePull", "CreateContainerError"}
    # Все отказы схемы store (registry.go, store.go): имена объектов схемы и версии, без строк.
    SCHEMA = re.compile(r"incompatible database schema: ((table|index|trigger|view) \w+ differs from current schema"
                        r"|missing (table|index|trigger|view) \w+|found \d+, want \d+"
                        r"|swarm primary owner has no owner grant|workflow revision counters differ from definitions)")
    FAILURES = re.compile(SCHEMA.pattern + r"|snapshot sha256 does not match the verified snapshot"
                          r"|snapshot is too large to verify in memory"
                          # Отказы prune (store.PruneSnapshots): без имён файлов.
                          r"|cannot remove an old snapshot|cannot list the snapshot directory"
                          r"|current snapshot (must exist as a regular file|disappeared during pruning)")

    def await_helper(self, operation, name, expected, uid, timeout=190, interval=2):
        """Ждёт любой терминальный статус helper; Failed - сразу, а не по таймауту."""
        deadline = self.clock() + timeout
        while True:
            self.beat()
            actual = self.deployment.get_object("pod", name)
            self.validate(actual, expected, uid)
            phase = actual.get("status", {}).get("phase")
            if phase == "Succeeded":
                return
            if phase == "Failed":
                refusal = RetentionRefused if operation == "prune" else RuntimeError
                raise refusal(self.failure(operation, name, actual))
            if self.clock() >= deadline:
                raise RuntimeError(f"owned database helper {operation} did not finish within {timeout}s "
                                   f"(phase={phase if phase in {'Pending', 'Running'} else 'unknown'}); "
                                   "no automatic database restore is attempted")
            self.sleep(interval)

    def failure(self, operation, name, pod):
        status = pod.get("status", {})
        statuses = [c for c in status.get("containerStatuses", []) if c.get("name") == "database"]
        terminated = (statuses[0].get("state", {}).get("terminated", {}) if statuses else {})
        reason = terminated.get("reason") or status.get("reason")
        code = terminated.get("exitCode")
        parts = [f"owned database helper {operation} failed",
                 f"reason={reason if reason in self.REASONS else 'unknown'}"]
        if type(code) is int:
            parts.append(f"exit_code={code}")
        detail = self.diagnostic(name)
        message = ", ".join(parts)
        if detail:
            message += f": {detail}"
        if operation == "verify" and self.SCHEMA.fullmatch(detail):
            # Доказанная несовместимость: apply не начинался, сервер на прежнем образе.
            return (message + "; the release image cannot open the current database, so the rollout is refused "
                    "before apply and deployment/brood-go keeps its current image; recreating the database is a "
                    "separate operator decision (docs/DEPLOY.md)")
        return message + "; no automatic database restore is attempted"

    def diagnostic(self, name):
        """Только совпадение с фиксированной грамматикой; остальной вывод отбрасывается."""
        code, out, _ = self.deployment.cluster.shell.output(
            "kubectl", f"--context={local.CONTEXT}", "-n", "brood", "logs", name, "-c", "database",
            "--limit-bytes=4096", "--request-timeout=15s")
        found = None if code else self.FAILURES.search(out)
        return found.group(0) if found else ""

    def snapshot(self, image):
        try:
            self.recheck()  # отказ по идентичности - до единственной записи (Lease)
            self.claim()
            self.protected_snapshot(image)
        except BaseException:
            self.release()
            raise
        self.handed_off = True  # дальше apply: возврат отсюда - последняя точка, где отказ безопасен

    def protected_snapshot(self, image):
        state = self.recheck()
        recovery = state.get("mode") == "recovery"
        if recovery:
            # Snapshot живого PVC доказывает схему, но не отсутствие писателя: CrashLoop
            # pod перезапускается сам. Поэтому сначала гасим именно его (#220).
            self.quiesce(state)
        self.before = self.execute(image, "snapshot")
        self.guard()
        # Gate любого existing-запуска, не только recovery (#220): новый образ обязан
        # принять именно этот snapshot до первого apply сервера. Иначе apply уводит
        # единственный pod в CrashLoop на несовместимой схеме, как при выкладке #206.
        verified = self.execute(image, "verify")
        if verified.get("verified") is not True or verified.get("schema_version") != self.before["schema_version"]:
            raise RuntimeError("verified schema version does not match the snapshot; refusing rollout before apply")
        self.guard()
        self.recovered = recovery

    # Закрытое множество фаз, в которых pod уже не может писать в том.
    INERT = {"Succeeded", "Failed"}

    def mounting_pods(self):
        """Все не завершённые pod namespace, монтирующие PVC данных, а не только app=brood-go."""
        raw = self.command("get", "pods", "-o", "json")
        try:
            items = json.loads(raw)["items"]
            return [p for p in items
                    if p.get("status", {}).get("phase") not in self.INERT
                    and any(v.get("persistentVolumeClaim", {}).get("claimName") == "brood-go-data"
                            for v in p.get("spec", {}).get("volumes", []))]
        except (ValueError, KeyError, TypeError, AttributeError):
            raise RuntimeError("cannot list pods mounting the database volume; refusing rollout") from None

    def exclusive_volume(self, allow=frozenset(), expect=None):
        """Отказ, если том монтирует любой pod кроме разрешённых по UID."""
        pods = self.mounting_pods()
        extra = [p for p in pods if p["metadata"].get("uid") not in allow]
        if extra or (expect is not None and not any(p["metadata"].get("uid") == expect for p in pods)):
            raise RuntimeError("database volume is mounted by an unexpected pod; refusing rollout")
        return pods

    # Запись о нашем scale-to-zero живёт в самом Deployment: аннотация пишется ДО
    # изменения replicas тем же CAS (uid + resourceVersion), поэтому оборванный
    # процесс оставляет следующему запуску доказуемое намерение, а не догадку по имени.
    NOTE = "brood.dev/quiesce"
    # Выше helper deadline (180 с) и request-timeout: моложе записи не трогаем. Это только
    # нижняя граница "запрос ещё в пути", а не доказательство, что запуск мёртв: живой запуск
    # длится дольше (см. Lease ниже); ни часы, ни аренда не доказывают, что запуск мёртв.
    GRACE = 240
    PROCEDURE = ("nothing was changed; follow the manual lease removal and quiesce "
                 "recovery procedures in docs/DEPLOY.md before rerunning the deploy")
    # Для отказов после возможных записей: фразы "nothing was changed" здесь быть не может.
    RELEASE_PROCEDURE = ("the deploy may already have written to the cluster; check whether an ambiguous "
                         "apply or restore went through, then follow the manual lease removal and quiesce "
                         "recovery procedures in docs/DEPLOY.md before rerunning the deploy")
    # phase: intent - записано до scale; scaled - пишется тем же patch, что и replicas=0, поэтому
    # (replicas=0, phase=scaled) существуют только вместе и только от нашего scale.
    PHASES = {"intent", "scaled"}
    KEYS = {"v", "id", "uid", "phase", "image", "pod", "spec", "fence", "at"}

    @staticmethod
    def spec_digest(spec, replicas):
        wanted = copy.deepcopy(spec)
        wanted["replicas"] = replicas
        return hashlib.sha256(json.dumps(wanted, sort_keys=True, separators=(",", ":")).encode()).hexdigest()

    @staticmethod
    def nonfinite(_):
        raise ValueError

    def parse_record(self, raw, uid):
        """Строгий разбор записи; чужая, повреждённая или от другого Deployment - отказ."""
        try:
            record = json.loads(raw, parse_constant=self.nonfinite)
            if (not isinstance(record, dict) or set(record) != self.KEYS or record["v"] != 1
                    or not re.fullmatch(r"[0-9a-f]{32}", record["id"]) or record["uid"] != uid
                    or record["phase"] not in self.PHASES
                    or type(record["fence"]) is not int or record["fence"] < 0
                    or not isinstance(record["image"], str) or not isinstance(record["pod"], str)
                    or not re.fullmatch(r"[0-9a-f]{64}", record["spec"]) or type(record["at"]) not in {int, float}
                    # NaN/Inf и 1e999 ломают сравнение возраста; запись из будущего была бы
                    # "молодой" вечно, поэтому допуск вперёд ограничен GRACE.
                    or not math.isfinite(record["at"]) or not 0 <= record["at"] <= self.wall() + self.GRACE):
                raise ValueError
            return record
        except (ValueError, TypeError, KeyError, OverflowError):
            raise RuntimeError("deployment/brood-go carries a quiesce record that cannot be verified; "
                               + self.PROCEDURE) from None

    def note(self, record):
        return json.dumps(record, sort_keys=True, separators=(",", ":"))

    def record_of(self, deployment):
        raw = deployment["metadata"].get("annotations", {}).get(self.NOTE)
        return None if raw is None else self.parse_record(raw, deployment["metadata"]["uid"])

    def classify(self, deployment, record):
        """scaled: наш scale подтверждён (replicas=0, phase=scaled) и spec никто не трогал;
        intent: только намерение, replicas ещё 1, spec прежний; idle: реплики вернули (apply,
        оператор) или spec сменили, запись устарела; foreign: replicas=0, но не наше состояние.
        generation не участвует: apiserver увеличивает его на любую смену spec и аннотаций,
        в том числе на нашу собственную запись, и считать его по шагам нельзя."""
        meta, spec = deployment["metadata"], deployment["spec"]
        if meta.get("uid") != record["uid"]:
            return "foreign"
        same = self.spec_digest(spec, 0) == record["spec"]
        if spec.get("replicas") == 0:
            return "scaled" if same and record["phase"] == "scaled" else "foreign"
        return "intent" if spec.get("replicas") == 1 and same and record["phase"] == "intent" else "idle"

    def write(self, current, note, replicas=None):
        """Один CAS-patch: uid и resourceVersion едут в теле, ответ обязан вернуть тот же uid.
        Любая ошибка или непарсируемый ответ - 'исход неизвестен', решает повторное чтение."""
        self.require_owner()
        meta = current["metadata"]
        patch = {"metadata": {"uid": meta["uid"], "resourceVersion": meta["resourceVersion"],
                              "annotations": {self.NOTE: note}}}
        if replicas is not None:
            patch["spec"] = {"replicas": replicas}
        raw = self.command("patch", "deployment", "brood-go", "--type=merge", "-p", json.dumps(patch), "-o", "json")
        try:
            result = json.loads(raw)
            if result["metadata"]["uid"] != meta["uid"]:
                raise ValueError
            return result
        except (ValueError, KeyError, TypeError):
            raise RuntimeError("deployment patch result does not match its preconditions") from None

    # kubectl не отдаёт Status структурно: stderr - строка, а не машинный Status, поэтому
    # полной гарантии она не даёт, только необходимое условие. Принимается ровно один
    # ожидаемый текст на случай, с заранее вычисленным чужим UID и настоящим именем Deployment;
    # любая другая строка (webhook, таймаут, сеть, иной UID, иной префикс) - непройденная проба.
    # Приём "Error from server (Conflict)" для устаревшего RV - прежний; реальный kubectl 1.36
    # на чужой UID печатает Invalid БЕЗ префикса "Error from server".
    ALIEN_UID = "00000000-0000-0000-0000-000000000000"
    PROBE_STALE = (r'Error from server \(Conflict\): Operation cannot be fulfilled on deployments\.apps '
                   r'"brood-go": the object has been modified; please apply your changes to the latest '
                   r"version and try again")

    @classmethod
    def probe_uid_text(cls):
        return (f'The Deployment "brood-go" is invalid: metadata.uid: Invalid value: "{cls.ALIEN_UID}": '
                "field is immutable")

    @classmethod
    def refused_by(cls, err, kind):
        if kind == "stale":
            return re.fullmatch(cls.PROBE_STALE + r"\n?", err or "") is not None
        return (err or "") in {cls.probe_uid_text(), cls.probe_uid_text() + "\n"}

    def probe_preconditions(self, current):
        """Живое доказательство, что именно этот API применяет uid/resourceVersion в merge-patch:
        server-side dry-run со старым RV и чужим UID обязан отказать конфликтом. Ничего не пишет.
        Отказ засчитывается только по reason и точному сообщению apiserver (см. PROBE_*)."""
        meta = current["metadata"]
        if meta["uid"] == self.ALIEN_UID:
            raise RuntimeError("API did not prove enforcement of uid/resourceVersion preconditions; "
                               "scale-to-zero recovery is blocked, nothing was changed")
        cases = (("1", meta["uid"], "stale"), (meta["resourceVersion"], self.ALIEN_UID, "uid"))
        for version, uid, kind in cases:
            body = {"metadata": {"uid": uid, "resourceVersion": version}, "spec": {"replicas": 1}}
            code, _, err = self.deployment.cluster.shell.output(
                "kubectl", f"--context={local.CONTEXT}", "-n", "brood", "patch", "deployment", "brood-go",
                "--type=merge", "--dry-run=server", "-p", json.dumps(body), "-o", "json", "--request-timeout=15s")
            if code == 0 or not self.refused_by(err, kind):
                raise RuntimeError("API did not prove enforcement of uid/resourceVersion preconditions; "
                                   "scale-to-zero recovery is blocked, nothing was changed")

    def quiesce(self, state, steps=8):
        """Единственный подтверждённый Deployment уводится в replicas=0 по протоколу:
        1) probe предусловий; 2) запись намерения (CAS, replicas не тронуты); 3) scale вместе с
        phase=scaled одним CAS от свежего resourceVersion. Каждый круг начинается с чтения и
        классификации по записи в самом объекте, а успехом считается только прочитанное
        состояние 'scaled', не ответ patch: потерянный ответ разрешается сам. 'Scale не
        применился' закрепляется fence-записью: она меняет resourceVersion, и запоздавший
        patch со старым RV получает конфликт."""
        recovery = state["recovery"]
        record = pod_name = baseline = None
        self.claim()
        for step in range(steps):
            self.beat()
            last = step == steps - 1
            # Писатель обязан быть на месте только до первого подтверждённого scale: потом он уже
            # мог исчезнуть, и это ожидаемо.
            pods = self.exclusive_volume(allow={recovery["pod"]}, expect=None if record else recovery["pod"])
            pod_name = pod_name or next(p["metadata"]["name"] for p in pods if p["metadata"]["uid"] == recovery["pod"])
            current = self.owned("deployment", "brood-go")
            meta = current["metadata"]
            existing = self.record_of(current)
            if existing is not None and (record is None or existing["id"] != record["id"]):
                raise RuntimeError("deployment/brood-go carries a quiesce record this deploy did not write; "
                                   + self.PROCEDURE)
            if existing is None:
                # Намерение ещё не подтверждено (первый круг или потерянная запись): весь
                # прежний образ Deployment доказывается заново, и только потом пишется запись.
                if (meta["uid"] != state["identities"]["deployment"] or current["spec"].get("replicas") != 1
                        or self.spec_digest(current["spec"], 0) != recovery["spec"] or not meta.get("resourceVersion")
                        or current["spec"]["template"]["spec"]["containers"][0]["image"] != recovery["image"]
                        or self.database_path(current["spec"]["template"]["spec"]) != state["path"]):
                    raise RuntimeError("existing deployment identity changed before database helper")
                if record is None:
                    self.probe_preconditions(current)
                    record = {"v": 1, "id": uuid.uuid4().hex, "uid": meta["uid"], "phase": "intent",
                              "image": recovery["image"], "pod": recovery["pod"], "fence": 0, "at": self.wall(),
                              "spec": recovery["spec"]}
                    self.quiesced = {"uid": meta["uid"], "id": record["id"], "spec": None}
                kind = "none"
            else:
                record = existing
                kind = self.classify(current, record)
            if kind == "scaled":
                generation = meta.get("generation")
                # Ожидаемый generation доказан одним шагом: чтение, от которого шёл CAS, плюс наша
                # одна запись. Любая другая правка spec или аннотаций сдвинула бы его дальше.
                if type(generation) is not int or baseline is None or generation != baseline:
                    raise RuntimeError("deployment/brood-go generation does not match the one write this deploy "
                                       "made; something else changed it, so the state cannot be proven; "
                                       "refusing rollout")
                self.quiesced.update(spec=current["spec"], generation=generation)
                self.await_gone(pod_name, recovery["pod"])
                self.guard()
                return
            if kind not in {"none", "intent"}:
                raise RuntimeError("existing deployment identity changed before database helper")
            if last:
                break
            try:
                if kind == "none":
                    baseline = None
                    self.write(current, self.note(record))  # намерение подтверждается ДО replicas
                else:
                    scaled = self.note(dict(record, phase="scaled"))
                    baseline = meta["generation"] + 1 if type(meta.get("generation")) is int else None
                    reply = self.write(current, scaled, replicas=0)
                    # Ответ уточняет, но не подменяет чтение: новый RV, наш token, ожидаемый generation.
                    if (reply["metadata"].get("resourceVersion") in {None, meta["resourceVersion"]}
                            or reply["metadata"].get("annotations", {}).get(self.NOTE) != scaled
                            or reply["metadata"].get("generation") != baseline):
                        baseline = None
            except RuntimeError:
                pass  # исход неизвестен: следующий круг прочитает и решит по факту
            if kind == "intent" and not self.fence_late(record):
                raise RuntimeError("deployment scale outcome could not be fenced; refusing rollout; "
                                   + self.PROCEDURE)
        if record is not None and kind == "intent":
            self.fence_late(record)  # висящий scale не должен остаться живым
        raise RuntimeError("deployment scale-to-zero could not be confirmed; refusing rollout")

    def fence_late(self, record):
        """После scale (любой исход): если по свежему чтению replicas всё ещё 1, CAS-запись с
        fence+1 меняет resourceVersion - запись всегда меняет аннотацию, значит не no-op, а no-op
        apiserver оставил бы RV прежним. Запоздавший scale со старым RV после неё применяться не
        может; если он успел раньше, наша запись получит конфликт. Истинно, только когда чтение
        подтвердило fence или иное состояние записи; неподтверждённый fence - ложь."""
        fresh = self.deployment.get_object("deployment", "brood-go")
        try:
            again = self.record_of(fresh)
            if not again or again["id"] != record["id"]:
                return False
            if self.classify(fresh, again) != "intent":
                return True  # scaled или запись уже снята/устарела: писать нечего
            try:
                self.write(fresh, self.note(dict(again, fence=again["fence"] + 1)))
            except RuntimeError:
                pass  # исход неизвестен: решает чтение ниже
            after = self.deployment.get_object("deployment", "brood-go")
            seen = self.record_of(after)
            if not seen or seen["id"] != record["id"]:
                return False
            return self.classify(after, seen) != "intent" or seen["fence"] > again["fence"]
        except RuntimeError:
            return False

    def await_gone(self, name, uid, timeout=120, interval=2):
        deadline = self.clock() + timeout
        while True:
            self.beat()
            pod = self.deployment.get_object("pod", name)
            if pod is None or pod["metadata"].get("uid") != uid:
                return
            if self.clock() >= deadline:
                raise RuntimeError("crash-looping pod did not terminate after scale to zero; refusing rollout")
            self.sleep(interval)

    def fence(self):
        """Состояние после scale: те же Deployment, Service, PVC, PV; Deployment не тронут никем
        кроме нас: spec целиком, наша запись и generation, зафиксированный после подтверждённого
        scale (apiserver поднимает его на любую смену spec и аннотаций, так что A->B->A, scale 1->0
        и чужая аннотация между guard видны; RV не сравнивается, его двигает статус контроллера).
        Нашей записи после scale нет: появится - обновлять ожидание только по ответу с проверкой
        RV и token. Ни одного pod с томом."""
        self.require_owner()
        quiet = self.quiesced
        deployment = self.owned("deployment", "brood-go")
        meta, state = deployment["metadata"], self.state
        record = self.record_of(deployment)
        if quiet.get("generation") is not None and meta.get("generation") != quiet["generation"]:
            raise RuntimeError("deployment/brood-go generation moved after the confirmed scale: spec or annotations "
                               "were written by someone else; refusing database access")
        if (meta["uid"] != quiet["uid"] or deployment["spec"].get("replicas") != 0
                or (quiet["spec"] is not None and deployment["spec"] != quiet["spec"])
                or (quiet.get("generation") is not None and meta.get("generation") != quiet["generation"])
                or not record or record["id"] != quiet["id"] or record["phase"] != "scaled"):
            raise RuntimeError("existing deployment identity changed before database helper")
        service = self.owned("service", "brood-go")
        pvc = self.owned("pvc", "brood-go-data")
        volume = self.deployment.get_object("pv", pvc["spec"]["volumeName"])
        claim = (volume or {}).get("spec", {}).get("claimRef", {})
        if (service["metadata"]["uid"] != state["identities"]["service"]
                or pvc["metadata"]["uid"] != state["identities"]["pvc"] or pvc.get("status", {}).get("phase") != "Bound"
                or not volume or volume["metadata"].get("uid") != state["volume"]
                or claim.get("uid") != pvc["metadata"]["uid"]):
            raise RuntimeError("existing deployment identity changed before database helper")
        self.exclusive_volume()

    def guard(self):
        """Перед каждым доступом к тому: живая аренда, затем recheck или, после scale, fence."""
        self.beat(force=True)
        if self.quiesced:
            self.fence()
            return self.state
        return self.recheck()

    def settle(self, mine=None, attempts=3):
        """Приводит Deployment с записью к чистому виду только по доказательствам записи.
        scaled -> replicas=1 и снятие записи одним CAS; intent/idle -> только снятие записи
        (CAS, он же закрепляет запоздавший patch); foreign -> ничего не трогаем. Успех - только
        прочитанное состояние (записи нет, replicas как задумано), не ответ patch.
        Возвращает None (записи нет), 'cleared' или 'restored'."""
        self.require_owner()
        planned = None
        for attempt in range(attempts + 1):
            current = self.deployment.get_object("deployment", "brood-go")
            if not current or not current.get("metadata", {}).get("uid") or not current["metadata"].get("resourceVersion"):
                raise RuntimeError("deployment/brood-go cannot be read; " + self.PROCEDURE)
            record = self.record_of(current)
            if record is None:
                if planned:
                    if current["spec"].get("replicas") != planned[1]:
                        raise RuntimeError("deployment restore result does not match its preconditions")
                    return planned[0]
                if current["spec"].get("replicas") == 0 and mine:
                    raise RuntimeError("quiesce record was removed while the Deployment is at replicas=0; "
                                       + self.PROCEDURE)
                return None
            if mine and record["id"] != mine:
                raise RuntimeError("quiesce record belongs to another deploy; " + self.PROCEDURE)
            kind = self.classify(current, record)
            if kind == "foreign":
                raise RuntimeError("deployment/brood-go is at replicas=0 but its spec or quiesce phase is not the "
                                   "state this deploy recorded; " + self.PROCEDURE)
            if attempt == attempts:
                break
            wanted = 1 if kind == "scaled" else current["spec"].get("replicas")
            try:
                self.write(current, None, replicas=1 if kind == "scaled" else None)
            except RuntimeError:
                pass  # исход неизвестен: следующее чтение решает
            planned = ("restored" if kind == "scaled" else "cleared", wanted)
        raise RuntimeError("deployment/brood-go restore could not be confirmed")

    # Аренда параллельного запуска. Автоматического перехвата НЕТ: возраст Lease и "она не менялась
    # N секунд" не доказывают, что владелец мёртв. Процесс может зависнуть после последнего guard
    # и позже выполнить нефенсированный apply; heartbeat не делает "guard -> apply" атомарным.
    # Поэтому любая существующая Lease brood-go-deploy - отказ до записи, независимо от возраста
    # и держателя. Её снимает только оператор по процедуре из docs/DEPLOY.md после доказанной
    # смерти прежнего процесса. Heartbeat остаётся лишь для обнаружения ручной подмены.
    LEASE = "brood-go-deploy"
    # Информативно: Lease не истекает автоматически, по этому полю её никто не берёт.
    LEASE_SECONDS = 120
    BEAT = 20

    @staticmethod
    def micro(moment):
        return datetime.datetime.fromtimestamp(moment, datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ")

    def lease_notes(self):
        """Кто держит Lease: по этим полям оператор доказывает смерть процесса (docs/DEPLOY.md)."""
        notes = {"brood.dev/hostname": socket.gethostname(), "brood.dev/pid": str(os.getpid()),
                 "brood.dev/started": self.micro(self.started)}
        release = os.environ.get("BROOD_RELEASE_SHA")
        if release:
            notes["brood.dev/release"] = release
        return notes

    def lease_spec(self):
        return {"holderIdentity": self.holder, "leaseDurationSeconds": self.LEASE_SECONDS,
                "renewTime": self.micro(self.wall())}

    def lease_taken(self, raw):
        try:
            meta = json.loads(raw)["metadata"]
            state = {"uid": meta["uid"], "rv": meta["resourceVersion"]}
        except (ValueError, KeyError, TypeError):
            raise RuntimeError("deploy lease reply cannot be identified; refusing rollout") from None
        self.beaten = self.clock()
        return state

    def lease_write(self, current=None):
        """create (current=None) либо CAS-patch по uid и resourceVersion; None - не применилось."""
        if current is None:
            body = {"apiVersion": "coordination.k8s.io/v1", "kind": "Lease", "spec": self.lease_spec(),
                    "metadata": {"name": self.LEASE, "namespace": "brood", "labels": dict(OWNER),
                                 "annotations": self.lease_notes()}}
            with tempfile.TemporaryDirectory(prefix="brood-deploy-lease-") as directory:
                path = Path(directory) / "lease.json"
                path.write_text(json.dumps(body))
                code, out, _ = self.kubectl("create", "-f", str(path), "-o", "json")
        else:
            body = {"metadata": {"uid": current["uid"], "resourceVersion": current["rv"]}, "spec": self.lease_spec()}
            code, out, _ = self.kubectl("patch", "lease", self.LEASE, "--type=merge", "-p", json.dumps(body), "-o", "json")
        return None if code else self.lease_taken(out)

    def lock(self):
        """Неблокирующий flock на файле машины запуска; занят - отказ, не ожидание."""
        if self.lock_fd is not None:
            return
        os.makedirs(os.path.dirname(self.lock_path), mode=0o700, exist_ok=True)
        fd = os.open(self.lock_path, os.O_RDWR | os.O_CREAT | os.O_CLOEXEC, 0o600)
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            os.close(fd)
            raise RuntimeError("another deploy process holds the deploy lock on this machine; nothing was "
                               "changed") from None
        self.lock_fd = fd

    def unlock(self):
        fd, self.lock_fd = self.lock_fd, None
        if fd is not None:
            os.close(fd)

    def refuse_foreign_lease(self):
        """Любая Lease, кроме нашей, - отказ: чужая, старая и "мёртвая" неразличимы."""
        held = self.deployment.get_object("lease", self.LEASE)
        if held is not None and not (self.lease and self.mine(held)):
            raise RuntimeError("lease/" + self.LEASE + " exists; automatic takeover is not supported because "
                               "a stalled holder may still apply; " + self.PROCEDURE)

    def claim(self):
        """Единственный владелец запуска: flock машины, затем create Lease. Существующая Lease -
        отказ без единой записи; потерянный create не повторяется."""
        if self.lease:
            return
        self.lock()
        try:
            self.refuse_foreign_lease()
            self.lease = self.lease_write()
            if not self.lease:
                raise RuntimeError("deploy lease could not be created or its reply was lost; another deploy "
                                   "process may be running, or lease/" + self.LEASE + " may now exist: "
                                   "check it and follow the manual lease removal procedure in docs/DEPLOY.md "
                                   "before rerunning the deploy")
        except BaseException:
            self.unlock()
            raise

    def mine(self, lease):
        return (lease is not None and lease.get("metadata", {}).get("uid") == self.lease["uid"]
                and lease.get("spec", {}).get("holderIdentity") == self.holder)

    LOST = ("deploy lease was taken over by another process while this one was stalled; "
            "refusing to continue, nothing further was changed")

    def require_owner(self):
        if self.evicted:
            raise RuntimeError(self.LOST)

    def require_lease(self):
        """Перед записью без guard: в кластере всё ещё наша Lease, иначе вытеснение (чтение)."""
        if not self.lease or not self.mine(self.deployment.get_object("lease", self.LEASE)):
            self.evict()

    def evict(self):
        self.lease, self.evicted = None, True
        raise RuntimeError(self.LOST)

    def beat(self, force=False):
        """Продлевает нашу Lease; потеря (перехват после паузы процесса) - отказ."""
        self.require_owner()
        if not self.lease or (not force and self.clock() - self.beaten < self.BEAT):
            return
        renewed = self.lease_write(self.lease)
        if renewed:
            self.lease = renewed
            return
        current = self.deployment.get_object("lease", self.LEASE)
        if not self.mine(current):
            self.evict()
        self.lease["rv"] = current["metadata"]["resourceVersion"]  # ответ потерян, Lease ещё наша

    def drop_lease(self):
        """Снимает нашу Lease; провал удаления - ошибка: успех при оставшейся Lease ложь."""
        lease, self.lease = self.lease, None
        if not lease:
            self.unlock()
            return
        stuck = ("deploy lease lease/" + self.LEASE + " could not be removed and may still exist; "
                 + self.RELEASE_PROCEDURE)
        try:
            current = self.deployment.get_object("lease", self.LEASE)
            self.lease = lease
            if not self.mine(current):
                return
            with tempfile.TemporaryDirectory(prefix="brood-deploy-lease-") as directory:
                path = Path(directory) / "delete.json"
                path.write_text(json.dumps({"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": {
                    "uid": lease["uid"], "resourceVersion": current["metadata"]["resourceVersion"]}}))
                code, _, _ = self.kubectl("delete", "--raw",
                                          f"/apis/coordination.k8s.io/v1/namespaces/brood/leases/{self.LEASE}",
                                          "-f", str(path))
            if code:
                raise RuntimeError(stuck)
        except RuntimeError as error:
            raise RuntimeError(stuck) from error
        finally:
            self.lease = None
            self.unlock()

    def young(self, record):
        age = self.wall() - record["at"]
        if age < self.GRACE:
            raise RuntimeError(f"deployment/brood-go carries a quiesce record only {int(max(age, 0))}s old; "
                               f"another deploy may be running or a request may be in flight; retry after "
                               f"{self.GRACE}s, nothing was changed")

    def assess(self, deployment):
        """Read-only оценка записи для --check: те же отказы, что у recover (повреждённая,
        чужой uid, моложе GRACE, foreign), но без единой записи в кластер. Успех значит
        только 'full run разберёт запись сам'; установка в этом режиме не проверяется -
        это делает run после recover."""
        record = self.record_of(deployment)
        self.young(record)
        kind = self.classify(deployment, record)
        if kind == "foreign":
            raise RuntimeError("deployment/brood-go is at replicas=0 but its spec or quiesce phase is not the "
                               "state this deploy recorded; " + self.PROCEDURE)
        return {"mode": "recoverable", "record": kind}

    def recover(self):
        """Повторный вход после оборванного запуска (SIGKILL, потеря сети, таймаут CI).
        Нет записи - обычный путь. Запись моложе GRACE - отказ (мог быть живой запуск или
        запрос в пути). Иначе settle по доказательствам записи. После restored выкладка
        не продолжается: pod новый, CrashLoop-доказательства нужно собрать заново."""
        self.require_owner()
        current = self.owned("deployment", "brood-go")
        record = self.record_of(current)
        if record is None:
            return None
        self.young(record)
        self.claim()
        self.require_lease()
        try:
            outcome = self.settle()
        except RuntimeError as error:
            # Записи в Deployment могли пройти (потерянный ответ): release Lease не снимает.
            self.ambiguous = True
            if "docs/DEPLOY.md" in str(error):
                raise
            raise RuntimeError(f"{error}; {self.RELEASE_PROCEDURE}") from None
        if outcome == "restored":
            raise RuntimeError("an interrupted deploy had scaled deployment/brood-go to zero; replicas=1 was "
                               "restored with its recorded identity; rerun the deploy once the pod has "
                               "crash-restarted (restartCount >= 1)")
        return outcome

    def release(self):
        """Откат нашего scale: через settle по записи, а не по памяти процесса. Если
        apply уже вернул реплики, снимается только запись. Нельзя доказать - явный отказ, а
        Lease остаётся: следующий запуск не продолжит поверх неизвестного состояния. После
        before_apply исход apply неоднозначен: apply не повторяется, Lease остаётся оператору."""
        quiet, self.quiesced = self.quiesced, None
        try:
            self.require_owner()
            if quiet:
                self.require_lease()
                self.settle(mine=quiet["id"])
        except RuntimeError:
            if self.evicted:
                # Вытесненный исполнитель не пишет ничего, даже откат: решает оператор.
                raise RuntimeError(self.LOST + "; the Deployment was left as is; " + self.RELEASE_PROCEDURE) from None
            raise RuntimeError("deployment/brood-go may be left at replicas=0 with a quiesce record; "
                               + self.RELEASE_PROCEDURE + "; no automatic database restore is attempted") from None
        if not (self.handed_off or self.ambiguous):
            self.drop_lease()

    def retain(self, image, keep):
        """Хранение снимков (#247): после успешной выкладки, ещё под Lease, helper нового
        образа тем же путём, что snapshot, оставляет снимок этой выкладки и keep-1 самых
        новых прочих .brood-snapshot-<hex>.db на PVC. Отказ самого prune - данные, не
        отказ: выкладка прошла, лишь часть старых снимков могла остаться. Иной сбой
        (identity, удаление helper по UID, API, ОС) пробрасывается: helper с записываемым
        томом мог остаться, и это решает оператор, как после inspect. keep None - ничего."""
        if keep is None:
            return {"enabled": False}
        if not self.snapshot_path or not self.before:
            return {"enabled": True, "keep": keep, "error": "this deploy made no verified snapshot to keep"}
        try:
            return {"enabled": True, "keep": keep, **self.execute(image, "prune", keep=keep)}
        except RetentionRefused as error:
            return {"enabled": True, "keep": keep, "error": str(error)}

    def finish(self):
        """Все эффекты завершены (apply, inspect, edge, probe): только теперь Lease снимается."""
        self.drop_lease()

    def inspect(self, image):
        # Apply уже вернул реплики: fence больше не действует. Запись снимаем, если она
        # осталась; неудача не ломает выкладку - следующий запуск снимет её сам (idle).
        quiet, self.quiesced = self.quiesced, None
        if quiet:
            try:
                self.settle(mine=quiet["id"])
            except RuntimeError:
                pass
        deployment = self.owned("deployment", "brood-go")
        if deployment["spec"]["template"]["spec"]["containers"][0]["image"] != image:
            raise RuntimeError("post-rollout runtime does not match the exact release image")
        if self.state and self.state.get("mode") == "recovery":
            self.rebase()
        after = self.execute(image, "inspect")
        if (not self.before or after["users"] != self.before["users"]
                or after["enabled_admins"] != self.before["enabled_admins"]):
            raise RuntimeError("database user/admin counts changed; operator review required, no automatic restore")
        return after
