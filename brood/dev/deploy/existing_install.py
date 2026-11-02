"""Закрытый snapshot gate существующей Go-БД, без bootstrap и авторизации."""
import copy
import json
from pathlib import Path
import re
import tempfile
import uuid

from brood.dev.deploy.go_server import OWNER
from brood.dev.substrate import local


class ExistingInstall:
    def __init__(self, deployment):
        self.deployment = deployment
        self.state = None
        self.before = None
        self.snapshot_path = None

    def owned(self, kind, name):
        obj = self.deployment.get_object(kind, name)
        meta = (obj or {}).get("metadata", {})
        if (meta.get("name") != name or meta.get("namespace") != "brood" or not meta.get("uid")
                or any(meta.get("labels", {}).get(k) != v for k, v in OWNER.items())):
            raise RuntimeError(f"existing deployment requires owned {kind}/{name}")
        return obj

    def check(self):
        try:
            return self._check()
        except (KeyError, TypeError, StopIteration, AttributeError, ValueError):
            raise RuntimeError("existing deployment has unsupported runtime or database configuration") from None

    def _check(self):
        deployment = self.owned("deployment", "brood-go")
        service = self.owned("service", "brood-go")
        pvc = self.owned("pvc", "brood-go-data")
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
        if (not digest or not runtime.get("ready")
                or not runtime.get("imageID", "").endswith("@" + digest[1])
                or owner.get("kind") != "ReplicaSet" or owner.get("uid") != rs["metadata"].get("uid")
                or rs_owner.get("kind") != "Deployment" or rs_owner.get("uid") != deployment["metadata"]["uid"]
                or meta.get("namespace") != "brood" or not meta.get("uid")
                or any(meta.get("labels", {}).get(k) != v for k, v in OWNER.items())
                or status.get("phase") != "Running"
                or not any(c.get("type") == "Ready" and c.get("status") == "True" for c in status.get("conditions", []))
                or current.get("observedGeneration", 0) < deployment["metadata"].get("generation", 1)
                or current.get("updatedReplicas") != 1 or current.get("availableReplicas") != 1
                or self.database_path(pod["spec"]) != path
                or pod["spec"]["containers"][0]["image"] != template["containers"][0]["image"]
                or not pod["spec"].get("nodeName")):
            raise RuntimeError("existing deployment runtime identity or readiness mismatch")
        state = {"path": path, "node": pod["spec"]["nodeName"], "volume": volume["metadata"]["uid"],
                 "identities": {"deployment": deployment["metadata"]["uid"],
                                "service": service["metadata"]["uid"], "pvc": pvc["metadata"]["uid"]}}
        return state

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

    def manifests(self, image, state, operation, name, destination):
        if not re.fullmatch(r"localhost:5002/brood-go@sha256:[0-9a-f]{64}", image):
            raise RuntimeError("database helper requires the newly built digest-pinned Go image")
        if operation not in {"snapshot", "inspect"}:
            raise RuntimeError("unsupported database helper operation")
        labels = {**OWNER, "brood.dev/database-helper": name}
        metadata = {"name": name, "namespace": "brood", "labels": labels}
        args = ["--path", state["path"]]
        if operation == "snapshot":
            if not destination or not re.fullmatch(r"/data/\.brood-snapshot-[a-z0-9-]+\.db", destination):
                raise RuntimeError("invalid private database snapshot destination")
            args += ["--output", destination]
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
                "volumeMounts": [{"name": "data", "mountPath": "/data", "readOnly": operation == "inspect"}]}],
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

    def command(self, *args):
        code, out, _ = self.deployment.cluster.shell.output(
            "kubectl", f"--context={local.CONTEXT}", "-n", "brood", *args, "--request-timeout=15s")
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

    def execute(self, image, operation):
        state = self.recheck()
        name = "brood-db-" + uuid.uuid4().hex
        if operation == "snapshot":
            self.snapshot_path = "/data/.brood-snapshot-" + uuid.uuid4().hex + ".db"
        pod, policy = self.manifests(image, state, operation, name, self.snapshot_path)
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
                self.command("wait", "--for=jsonpath={.status.phase}=Succeeded", "pod/" + name, "--timeout=190s")
                self.validate(self.deployment.get_object("pod", name), pod, created[-1][1])
                return self.metadata(self.command("logs", name, "-c", "database", "--limit-bytes=4096"),
                                     snapshot=operation == "snapshot")
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

    def snapshot(self, image):
        self.before = self.execute(image, "snapshot")
        self.recheck()

    def inspect(self, image):
        deployment = self.owned("deployment", "brood-go")
        if deployment["spec"]["template"]["spec"]["containers"][0]["image"] != image:
            raise RuntimeError("post-rollout runtime does not match the exact release image")
        after = self.execute(image, "inspect")
        if (not self.before or after["users"] != self.before["users"]
                or after["enabled_admins"] != self.before["enabled_admins"]):
            raise RuntimeError("database user/admin counts changed; operator review required, no automatic restore")
        return after
