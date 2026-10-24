"""Сервер brood в кластере локального Substrate (brood deploy server).

Образ собирается из Dockerfile рядом и кладётся в реестр установки по
digest -- так Substrate кладёт свои образы из Dockerfile (ate-setup,
images.BuildDockerfileImage). Манифесты -- база kustomize в manifests/.
Выкладка копирует её во временный каталог, дописывает оверлей -- образ по
digest и Secret'ы из .env и secrets/ рабочей копии -- и применяет его
server-side apply. Хэш в имени сгенерированного Secret'а перекатывает под,
когда меняется .env или сертификат.

Модуль ничего не печатает: процессы идут через Shell установки, и их вывод
попадает туда, куда она его направит.
"""
import http.client
import json
import os
import shutil
import ssl
import tempfile
import time

from brood.common import config
from brood.dev.substrate import local

HERE = os.path.dirname(os.path.realpath(__file__))
MANIFESTS = os.path.join(HERE, "manifests")
DOCKERFILE = os.path.join(HERE, "Dockerfile")
NAMESPACE = "brood"
OWNER = ("app.kubernetes.io/managed-by", "brood")
IMAGE = "brood-server"
REPOSITORY = f"localhost:{local.REGISTRY_PORT}/{IMAGE}"
# Что из .env едет в кластер: серверу -- его настройки, учётка
# администратора -- только init-контейнеру, который её заводит. GITLAB_TOKEN
# и прочее остаются на машине.
SERVER_KEYS = ("DJANGO_SECRET_KEY", "DJANGO_ALLOWED_HOSTS", "POSTGRES_PASSWORD")
ADMIN_KEYS = ("DJANGO_SUPERUSER_USERNAME", "DJANGO_SUPERUSER_PASSWORD", "DJANGO_SUPERUSER_EMAIL")
OPTIONAL = ("DJANGO_SUPERUSER_EMAIL",)
# Сертификат и ключ -- секреты установки рядом с .env, в secrets/ рабочей
# копии; на стенд их кладёт выкладка из переменных CI (.ci/deploy.sh).
TLS = {"tls.crt": "secrets/tls.crt", "tls.key": "secrets/tls.key"}
ROLLOUT = ("statefulset/postgres", "deployment/brood-server")
EDGE_DEADLINE = 60


def settings(get):
    """Настройки -> (сервера, администратора). Чистая функция (get --
    config.get). Недостающие -- одним отказом со всеми именами."""
    missing = [k for k in SERVER_KEYS + ADMIN_KEYS if k not in OPTIONAL and not get(k)]
    if missing:
        raise RuntimeError(f"brood deploy server needs in .env: {', '.join(missing)}")
    return ({k: get(k) for k in SERVER_KEYS},
            {k: get(k) for k in ADMIN_KEYS if get(k)})


def env_file(values):
    """{имя: значение} -> env-файл kustomize. Чистая функция. Кавычек
    kustomize не снимает: значения сюда идут уже разобранные config, а
    перевод строки разорвал бы файл."""
    for key, value in values.items():
        if "\n" in value:
            raise RuntimeError(f"{key} in .env spans lines; a Secret value must be one line")
    return "".join(f"{key}={value}\n" for key, value in values.items())


def kustomization(digest):
    """Оверлей поверх base/ -> текст kustomization.yaml (JSON -- тоже YAML).
    Чистая функция."""
    return json.dumps({
        "apiVersion": "kustomize.config.k8s.io/v1beta1",
        "kind": "Kustomization",
        "resources": ["base"],
        "images": [{"name": IMAGE, "newName": REPOSITORY, "digest": digest}],
        "secretGenerator": [
            {"name": "brood-env", "namespace": NAMESPACE, "envs": ["server.env"]},
            {"name": "brood-admin", "namespace": NAMESPACE, "envs": ["admin.env"]},
            {"name": "brood-tls", "namespace": NAMESPACE, "type": "kubernetes.io/tls",
             "files": sorted(TLS)},
        ],
    }, indent=2) + "\n"


def first_host(allowed):
    """DJANGO_ALLOWED_HOSTS -> имя, которым проверять вход. Чистая функция."""
    hosts = [h.strip() for h in allowed.split(",") if h.strip()]
    if not hosts:
        raise RuntimeError("DJANGO_ALLOWED_HOSTS in .env names no host")
    return hosts[0]


class Deploy:
    """Выкладка сервера в кластер установки cluster (local.LocalCluster).
    Настройки, каталог рабочей копии, часы и ожидание передаются, чтобы
    шаги можно было гонять без сети и без сна."""

    def __init__(self, cluster, get=config.get, project=config.PROJECT,
                 clock=time.monotonic, sleep=time.sleep):
        self.cluster, self.shell = cluster, cluster.shell
        self.get, self.project = get, project
        self.clock, self.sleep = clock, sleep

    def server(self):
        """Собрать образ, раскатать сервер с базой и дождаться, пока он
        отвечает -> (ссылка на образ, адрес входа или None)."""
        server, admin = settings(self.get)
        tls = self._tls()
        self.cluster.check()
        self._namespace_is_ours()
        digest = self._image()
        workdir = tempfile.mkdtemp(prefix="brood-deploy.")
        try:
            self._overlay(workdir, digest, server, admin, tls)
            self._kubectl("apply", "--server-side", "--force-conflicts", "-k", workdir)
        finally:
            shutil.rmtree(workdir, ignore_errors=True)
        for what in ROLLOUT:
            self._kubectl("rollout", "status", what, "-n", NAMESPACE, "--timeout=600s")
        ref = f"{REPOSITORY}@{digest}"
        port = local.edge_ports(self.get).get(dict(local.EDGE)["BROOD_HTTPS_PORT"])
        if not port:
            return ref, None
        host = first_host(server["DJANGO_ALLOWED_HOSTS"])
        self._edge(port, host, tls["tls.crt"])
        return ref, f"https://{host}" + ("" if port == 443 else f":{port}") + "/admin/"

    # ─── шаги ────────────────────────────────────────────────────────────
    def _tls(self):
        """Сертификат и ключ из secrets/ -> {имя в Secret: текст}. Пара
        сверяется до выкладки: nginx с чужим ключом не поднялся бы, и под
        ждал бы готовности до таймаута."""
        paths = {name: os.path.join(self.project, rel) for name, rel in TLS.items()}
        missing = [TLS[name] for name, path in paths.items() if not os.path.isfile(path)]
        if missing:
            raise RuntimeError(f"brood deploy server needs the TLS certificate and key: "
                               f"no {', '.join(missing)} in {self.project}")
        try:
            ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER).load_cert_chain(paths["tls.crt"],
                                                                     paths["tls.key"])
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
        code, out, _ = self.shell.output(
            "kubectl", f"--context={local.CONTEXT}", "get", "namespace", NAMESPACE,
            "-o", "jsonpath={.metadata.labels." + key.replace(".", "\\.") + "}")
        if code == 0 and out.strip() != owner:
            raise RuntimeError(f"namespace {NAMESPACE} exists without label {key}={owner}: "
                               f"it is not brood's; refusing to deploy into it")

    def _image(self):
        """Собрать и запушить образ -> digest. Тег -- только имя, под которым
        пушить: в манифест едет digest, и устаревший тег не разрешится."""
        tag = f"{REPOSITORY}:build-{int(time.time())}"
        self.shell.run("docker", "buildx", "build", "--push", "-f", DOCKERFILE, "-t", tag,
                       self.project)
        code, out, err = self.shell.output("docker", "buildx", "imagetools", "inspect", tag,
                                           "--format", "{{json .Manifest}}")
        if code:
            raise RuntimeError(f"docker buildx imagetools inspect {tag}: {local.reason(err, code)}")
        digest = json.loads(out).get("digest", "")
        if not digest.startswith("sha256:"):
            raise RuntimeError(f"no digest for {tag} in the registry")
        return digest

    def _overlay(self, workdir, digest, server, admin, tls):
        os.chmod(workdir, 0o700)
        shutil.copytree(MANIFESTS, os.path.join(workdir, "base"))
        files = {"kustomization.yaml": kustomization(digest),
                 "server.env": env_file(server), "admin.env": env_file(admin), **tls}
        for name, text in files.items():
            fd = os.open(os.path.join(workdir, name), os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with os.fdopen(fd, "w", encoding="utf-8") as f:
                f.write(text)

    def _edge(self, port, host, cert):
        """Вход стенда отвечает: страница входа админки по https на порту
        хоста и ровно тот сертификат, что выложен. NodePort доезжает до
        kube-proxy не мгновенно -- поэтому с повтором."""
        want = ssl.PEM_cert_to_DER_cert(cert)
        context = ssl.create_default_context()
        context.check_hostname = False
        context.verify_mode = ssl.CERT_NONE
        deadline = self.clock() + EDGE_DEADLINE
        while True:
            try:
                conn = http.client.HTTPSConnection("127.0.0.1", port, context=context, timeout=5)
                try:
                    conn.request("GET", "/admin/login/", headers={"Host": host})
                    status = conn.getresponse().status
                    served = conn.sock.getpeercert(binary_form=True)
                finally:
                    conn.close()
                if served != want:
                    raise RuntimeError(f"https on port {port} serves another certificate than "
                                       f"{TLS['tls.crt']}")
                if status == 200:
                    return
                last = f"HTTP {status}"
            except OSError as e:
                last = str(e)
            if self.clock() >= deadline:
                raise RuntimeError(f"https://{host}/admin/login/ via 127.0.0.1:{port} did not "
                                   f"answer within {EDGE_DEADLINE} seconds: {last}")
            self.sleep(2)

    def _kubectl(self, *args):
        self.shell.run("kubectl", f"--context={local.CONTEXT}", *args)
