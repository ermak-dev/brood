"""Публичная Go-выкладка: initial bootstrap или явное сохранение текущей БД."""
import json
from pathlib import Path
import re
import subprocess
import tempfile
import uuid
from urllib.parse import urlsplit

from brood.common import config
from brood.dev.deploy import edge
from brood.dev.deploy.existing_install import ExistingInstall
from brood.dev.deploy.go_server import GIT_REMOTES, Deploy as GoDeploy, actor_model_environment, git_remote_hosts
from brood.dev.deploy.infrastructure import Infrastructure
from brood.dev.substrate import local


LEGACY = (("deployment", "django"), ("statefulset", "postgres"),
          ("service", "django"), ("service", "postgres"),
          ("networkpolicy", "django"), ("networkpolicy", "django-egress"),
          ("networkpolicy", "django-kube-api"), ("networkpolicy", "postgres"),
          ("pvc", "data-postgres-0"))


def settings(get):
    names = ("BROOD_SESSION_SECRET", "BROOD_ADMIN_USERNAME", "BROOD_ADMIN_PASSWORD",
             "BROOD_PUBLIC_HOST", "BROOD_HTTPS_PORT", "CLIPROXY_URL", "CLIPROXY_MANAGEMENT_KEY",
             "CLIPROXY_API_KEY")
    mode = get("BROOD_DEPLOY_MODE") or "initial"
    if mode not in {"initial", "existing"}:
        raise RuntimeError("BROOD_DEPLOY_MODE must be initial or existing")
    values = {key: get(key) or "" for key in names}
    values["BROOD_DEPLOY_MODE"] = mode
    required = names[:5] if mode == "initial" else ("BROOD_SESSION_SECRET", "BROOD_PUBLIC_HOST")
    for key in required:
        if not values[key] or any(char in values[key] for char in "\r\n\0"):
            raise RuntimeError(f"public Go deployment needs valid {key} in .env")
    # Ключ подписи сессий, а не пароль: длину пароля администратора выбирает оператор.
    if len(values["BROOD_SESSION_SECRET"]) < 32:
        raise RuntimeError("BROOD_SESSION_SECRET must contain at least 32 characters")
    if not re.fullmatch(r"[a-z0-9][a-z0-9.-]*[a-z0-9]", values["BROOD_PUBLIC_HOST"]):
        raise RuntimeError("BROOD_PUBLIC_HOST must be a DNS hostname")
    port = values["BROOD_HTTPS_PORT"]
    if not port.isdecimal() or not 0 < int(port) < 65536:
        raise RuntimeError("BROOD_HTTPS_PORT must be a valid port")
    if values["CLIPROXY_URL"]:
        target = urlsplit(values["CLIPROXY_URL"])
        if (target.scheme != "http" or target.hostname not in
                {"cliproxy", "cliproxy.brood.svc", "cliproxy.brood.svc.cluster.local"}
                or target.port != 8317 or target.username or target.password
                or target.path not in ("", "/") or target.query or target.fragment):
            raise RuntimeError("public deployment supports the installed CLIPROXY_URL service on port 8317")
        if not values["CLIPROXY_MANAGEMENT_KEY"]:
            raise RuntimeError("CLIPROXY_MANAGEMENT_KEY is required with CLIPROXY_URL")
    values.update(actor_model_environment(get))
    # Только форма, без DNS: отказ до сборки edge; значение берёт Go-выкладка.
    git_remote_hosts(get(GIT_REMOTES))
    return values


def identity(obj, kind, name):
    metadata = obj.get("metadata", {})
    if metadata.get("name") != name or metadata.get("namespace") != "brood" or not metadata.get("uid"):
        raise RuntimeError(f"cannot identify {kind}/{name}")
    owner = metadata.get("labels", {}).get("app.kubernetes.io/managed-by")
    if owner == "brood-go":
        return metadata["uid"]
    if owner not in (None, "brood"):
        raise RuntimeError(f"refusing foreign {kind}/{name}")
    spec = obj.get("spec", {})
    app = "django" if name.startswith("django") else name
    if kind in {"deployment", "statefulset"}:
        owned = spec.get("selector", {}).get("matchLabels") == {"app": app}
    elif kind == "service":
        owned = spec.get("selector") == {"app": app}
    elif kind == "networkpolicy":
        owned = spec.get("podSelector", {}).get("matchLabels") == {"app": app}
    elif (kind, name) == ("pvc", "data-postgres-0"):
        owned = metadata.get("labels", {}).get("app") == "postgres"
    else:
        owned = False
    if not owned:
        raise RuntimeError(f"refusing foreign {kind}/{name}")
    return metadata["uid"]


class Deployment:
    def __init__(self, cluster, get=config.get, project=config.PROJECT, runner=subprocess.run):
        self.cluster, self.get, self.project, self.runner = cluster, get, Path(project), runner

    def get_object(self, kind, name):
        code, out, _ = self.cluster.shell.output("kubectl", f"--context={local.CONTEXT}", "-n", "brood",
            "get", kind, name, "--ignore-not-found", "-o", "json", "--request-timeout=15s")
        if code:
            raise RuntimeError(f"cannot inspect {kind}/{name}")
        try:
            return json.loads(out) if out.strip() else None
        except ValueError:
            raise RuntimeError(f"cannot inspect {kind}/{name}: invalid JSON") from None

    def check(self, existing=None):
        """Preflight. existing - ExistingInstall полного запуска: только он (не --check)
        вправе перед проверкой установки разобрать оборванный scale-to-zero."""
        values = settings(self.get)
        self.cluster.check()
        infrastructure = Infrastructure(self.cluster, project=str(self.project))
        infrastructure._tls()
        infrastructure._namespace_is_ours()
        GoDeploy(self.cluster, get=self.get, project=self.project)._worker_images()
        infrastructure._api_egress()
        if values["BROOD_DEPLOY_MODE"] == "existing":
            if existing:
                existing.recover()
            if existing:
                existing.check()
            else:
                ExistingInstall(self).check(recoverable=True)
            for kind, name in (("deployment", "edge"), ("service", "edge")):
                ExistingInstall(self).owned(kind, name)
        old = {}
        for kind, name in (*LEGACY, ("deployment", "edge"), ("service", "edge")):
            obj = self.get_object(kind, name)
            if obj:
                old[(kind, name)] = identity(obj, kind, name)
        return values, old

    def build_edge(self):
        with tempfile.TemporaryDirectory(prefix="brood-edge-build-") as directory:
            metadata = Path(directory) / "metadata.json"
            tag = "localhost:5002/brood-go-edge:build-" + uuid.uuid4().hex
            code, _, _ = self.cluster.shell.output("docker", "buildx", "build", "--push", "--platform", "linux/amd64",
                "--metadata-file", str(metadata), "-t", tag, "-f",
                str(self.project / "brood/dev/deploy/edge/Dockerfile"), str(self.project))
            if code:
                raise RuntimeError("Go TLS edge image build failed")
            digest = json.loads(metadata.read_text()).get("containerimage.digest", "")
            if not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
                raise RuntimeError("Go TLS edge build returned no image digest")
            return "localhost:5002/brood-go-edge@" + digest

    def bootstrap(self, values):
        result = self.runner([self.cluster.kubectl, f"--context={local.CONTEXT}", "-n", "brood", "exec", "-i",
                             "deployment/brood-go", "--", "/brood", "bootstrap-admin"],
                            input=values["BROOD_ADMIN_USERNAME"] + "\n" + values["BROOD_ADMIN_PASSWORD"] + "\n",
                            capture_output=True, text=True, env=self.cluster.shell.env)
        if result.returncode:
            raise RuntimeError("Go administrator bootstrap failed; public ingress has not been switched")
        return isinstance(result.stdout, str) and " created" in result.stdout

    def apply_edge(self, image, before):
        objects = edge.manifests(image)
        for obj in objects:
            kind = obj["kind"].lower()
            current = self.get_object(kind, "edge")
            if current:
                uid = identity(current, kind, "edge")
                if before.get((kind, "edge")) not in (None, uid):
                    raise RuntimeError(f"identity changed for {kind}/edge")
                obj["metadata"]["uid"] = uid
                obj["metadata"]["resourceVersion"] = current["metadata"]["resourceVersion"]
                if kind == "service":
                    for key in ("clusterIP", "clusterIPs", "ipFamilies", "ipFamilyPolicy"):
                        if key in current["spec"]:
                            obj["spec"][key] = current["spec"][key]
        with tempfile.TemporaryDirectory(prefix="brood-public-edge-") as directory:
            path = Path(directory) / "edge.json"
            for obj in objects:
                path.write_text(json.dumps(obj))
                action = "replace" if "resourceVersion" in obj["metadata"] else "create"
                code, _, _ = self.cluster.shell.output("kubectl", f"--context={local.CONTEXT}", action, "-f", str(path))
                if code:
                    raise RuntimeError(f"Go TLS edge {action} failed")
        code, _, _ = self.cluster.shell.output("kubectl", f"--context={local.CONTEXT}", "-n", "brood",
                                             "rollout", "status", "deployment/edge", "--timeout=300s")
        if code:
            raise RuntimeError("Go TLS edge rollout failed")

    def probe(self, values, login):
        host, port = values["BROOD_PUBLIC_HOST"], values["BROOD_HTTPS_PORT"]
        authority = host if port == "443" else f"{host}:{port}"
        origin = f"https://{authority}"
        with tempfile.TemporaryDirectory(prefix="brood-public-check-") as directory:
            jar = Path(directory) / "cookies"
            jar.touch(mode=0o600)

            def request(path, status=200, body=None):
                args = ["curl", "--silent", "--show-error", "--max-time", "30", "--noproxy", "*",
                        "--cacert", str(self.project / "secrets/tls.crt"), "--resolve", f"{host}:{port}:127.0.0.1",
                        "--cookie", str(jar), "--cookie-jar", str(jar), "--write-out", "\n%{http_code}"]
                if body is not None:
                    csrf = ""
                    for line in jar.read_text().splitlines():
                        parts = line.split("\t")
                        if len(parts) == 7 and parts[5] == "csrf":
                            csrf = parts[6]
                    args += ["--header", "Content-Type: application/json", "--header", f"Origin: {origin}",
                             "--header", f"X-CSRFToken: {csrf}", "--data-binary", "@-"]
                result = self.runner([*args, origin + path], input=json.dumps(body) if body is not None else None,
                                     capture_output=True, text=True, env=self.cluster.shell.env)
                text, _, code = result.stdout.rpartition("\n")
                if result.returncode or code != str(status):
                    raise RuntimeError(f"public Go HTTPS check failed: {path} (expected {status})")
                return text

            request("/healthz")
            if "Brood Management Center" not in request("/"):
                raise RuntimeError("public endpoint is not serving the restored React panel")
            request("/api/auth/session")
            if login:
                request("/api/auth/login", 204, {"username": values["BROOD_ADMIN_USERNAME"],
                                                "password": values["BROOD_ADMIN_PASSWORD"]})
                session = json.loads(request("/api/auth/session"))
                if not session.get("authenticated") or not session.get("user", {}).get("is_admin"):
                    raise RuntimeError("new administrator session was not verified")
                if values["CLIPROXY_URL"]:
                    request("/v8/management/config")
                request("/api/auth/logout", 204, {})

    def cleanup(self, before):
        for kind, name in LEGACY:
            if (kind, name) not in before:
                continue
            obj = self.get_object(kind, name)
            if not obj:
                continue
            if identity(obj, kind, name) != before[(kind, name)]:
                raise RuntimeError(f"identity changed for {kind}/{name}; refusing cleanup")
            code, _, _ = self.cluster.shell.output("kubectl", f"--context={local.CONTEXT}", "-n", "brood",
                "delete", kind, name, "--wait=true", "--timeout=180s")
            if code:
                raise RuntimeError(f"cannot remove obsolete {kind}/{name}")

    def run_existing(self, existing):
        """Единый вход записи: flock и Lease берутся до первого действия, в том числе сборки."""
        try:
            existing.claim()
            values, before = self.check(existing)
            image = self.build_edge()
            existing.state = existing.check()
            server = GoDeploy(self.cluster, get=self.get, project=self.project).run(before_apply=existing.snapshot)
            existing.inspect(server)
            self.apply_edge(image, before)
            self.probe(values, login=False)
        except BaseException:
            # Отказ до before_apply откатывает scale; после - Lease остаётся (неоднозначный исход).
            existing.release()
            raise
        existing.finish()
        result = {"image": server, "edge": image, "host": values["BROOD_PUBLIC_HOST"],
                  "admin_created": False, "snapshot": existing.snapshot_path,
                  "acceptance": "operator-authenticated acceptance pending"}
        if existing.recovered:
            result["recovered"] = "crashloop"
        return result

    def run(self):
        existing = ExistingInstall(self) if (self.get("BROOD_DEPLOY_MODE") or "initial") == "existing" else None
        if existing:
            return self.run_existing(existing)
        values, before = self.check()
        image = self.build_edge()
        server = GoDeploy(self.cluster, get=self.get, project=self.project).run()
        created = self.bootstrap(values)
        self.apply_edge(image, before)
        self.probe(values, login=True)
        self.cleanup(before)
        return {"image": server, "edge": image, "host": values["BROOD_PUBLIC_HOST"],
                "admin": values["BROOD_ADMIN_USERNAME"], "admin_created": created}
