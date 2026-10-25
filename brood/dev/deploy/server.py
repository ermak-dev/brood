"""Сервер brood в кластере локального Substrate (brood deploy server).

Три Deployment за одним входом: edge (nginx с TLS и панелью), django и
cliproxy (CLIProxyAPI), плюс postgres. Образы brood -- сервер и вход --
собираются из Dockerfile'ов рядом и кладутся в реестр установки по digest,
так Substrate кладёт свои образы из Dockerfile (ate-setup,
images.BuildDockerfileImage). Манифесты -- база kustomize в manifests/.
Выкладка копирует её во временный каталог, дописывает оверлей -- образы по
digest и Secret'ы из .env и secrets/ рабочей копии -- и применяет его
server-side apply. Хэш в имени сгенерированного Secret'а перекатывает поды,
когда меняется .env или сертификат. Образ gvisor-воркера собирается pinned ko
из checkout'а Substrate; начальный WorkerPool создаётся отдельно, без apply,
чтобы выкладка не сбрасывала пользовательские правки и версии пулов.

Модуль ничего не печатает: процессы идут через Shell установки, и их вывод
попадает туда, куда она его направит.
"""
import http.client
import ipaddress
import json
import os
import re
import shutil
import ssl
import tempfile
import time

from brood.common import config
from brood.dev.substrate import local

HERE = os.path.dirname(os.path.realpath(__file__))
MANIFESTS = os.path.join(HERE, "manifests")
NAMESPACE = "brood"
OWNER = ("app.kubernetes.io/managed-by", "brood")
REGISTRY = f"localhost:{local.REGISTRY_PORT}"
# Образ в манифестах -> его Dockerfile.
IMAGES = {"brood-server": os.path.join(HERE, "Dockerfile"),
          "brood-edge": os.path.join(HERE, "edge", "Dockerfile")}
# Что из .env едет в кластер: серверу -- его настройки, учётка
# администратора -- только init-контейнеру, который её заводит. GITLAB_TOKEN
# и прочее остаются на машине.
SERVER_KEYS = ("DJANGO_SECRET_KEY", "DJANGO_ALLOWED_HOSTS", "POSTGRES_PASSWORD")
ADMIN_KEYS = ("DJANGO_SUPERUSER_USERNAME", "DJANGO_SUPERUSER_PASSWORD", "DJANGO_SUPERUSER_EMAIL")
OPTIONAL = ("DJANGO_SUPERUSER_EMAIL",)
# Ключи CLIProxyAPI: management -- его ставит только вход, клиентский -- для
# /v1, первым в config.yaml.
CLIPROXY_KEYS = ("CLIPROXY_MANAGEMENT_KEY", "CLIPROXY_CLIENT_KEY")
# Сертификат и ключ -- секреты установки рядом с .env, в secrets/ рабочей
# копии; на стенд их кладёт выкладка из переменных CI (.ci/deploy.sh).
TLS = {"tls.crt": "secrets/tls.crt", "tls.key": "secrets/tls.key"}
ROLLOUT = ("statefulset/postgres", "deployment/django", "deployment/cliproxy",
           "deployment/edge")
# Раскладка до входа отдельным Deployment'ом: под с Django и nginx и его
# NodePort-Service. Их NodePort'ы нужны edge, поэтому они уходят до apply.
LEGACY = ("deployment/brood-server", "service/brood-server")
EDGE_DEADLINE = 60


def settings(get):
    """Настройки -> (сервера, администратора, прокси). Чистая функция (get --
    config.get). Недостающие -- одним отказом со всеми именами."""
    every = SERVER_KEYS + ADMIN_KEYS + CLIPROXY_KEYS
    missing = [k for k in every if k not in OPTIONAL and not get(k)]
    if missing:
        raise RuntimeError(f"brood deploy server needs in .env: {', '.join(missing)}")
    return ({k: get(k) for k in SERVER_KEYS},
            {k: get(k) for k in ADMIN_KEYS if get(k)},
            {k: get(k) for k in CLIPROXY_KEYS})


def env_file(values):
    """{имя: значение} -> env-файл kustomize. Чистая функция. Кавычек
    kustomize не снимает: значения сюда идут уже разобранные config, а
    перевод строки разорвал бы файл."""
    for key, value in values.items():
        if "\n" in value:
            raise RuntimeError(f"{key} in .env spans lines; a Secret value must be one line")
    return "".join(f"{key}={value}\n" for key, value in values.items())


def kustomization(digests):
    """Оверлей поверх base/ ({образ: digest}) -> текст kustomization.yaml
    (JSON -- тоже YAML). Чистая функция."""
    return json.dumps({
        "apiVersion": "kustomize.config.k8s.io/v1beta1",
        "kind": "Kustomization",
        "resources": ["base", "api-egress.json"],
        "images": [{"name": name, "newName": f"{REGISTRY}/{name}", "digest": digest}
                   for name, digest in sorted(digests.items())],
        "secretGenerator": [
            {"name": "brood-env", "namespace": NAMESPACE, "envs": ["server.env"]},
            {"name": "brood-admin", "namespace": NAMESPACE, "envs": ["admin.env"]},
            {"name": "brood-cliproxy", "namespace": NAMESPACE, "envs": ["cliproxy.env"]},
            {"name": "brood-tls", "namespace": NAMESPACE, "type": "kubernetes.io/tls",
             "files": sorted(TLS)},
        ],
    }, indent=2) + "\n"


def initial_pool(image):
    """Начальный пул: только создание, не часть применяемой базы kustomize."""
    return {
        "apiVersion": "ate.dev/v1", "kind": "WorkerPool",
        "metadata": {"name": "brood", "namespace": NAMESPACE},
        "spec": {"replicas": 1, "workerImage": image,
                 "sandboxClasses": [{"name": "gvisor"}],
                 "template": {"resources": {
                     "requests": {"cpu": "250m", "memory": "2Gi"},
                     "limits": {"cpu": "2", "memory": "2Gi"}}}}}


def api_egress(service, endpoints):
    """Точные адреса API до и после DNAT: Service:443 и endpoints:6443.

    Неудачное discovery не расширяет доступ до всей сети или всех портов.
    Порты берутся из живых объектов, а не из предположения о kind."""
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
        """Собрать образы, раскатать сервер, прокси и вход с базой и
        дождаться, пока вход отвечает -> ({образ: digest}, адрес или None)."""
        server, admin, cliproxy = settings(self.get)
        tls = self._tls()
        self.cluster.check()
        self._namespace_is_ours()
        api_rules = self._api_egress()
        digests = {name: self._image(name, dockerfile) for name, dockerfile in IMAGES.items()}
        worker_image = self._worker_image()
        server["WORKER_POOL_IMAGE"] = worker_image
        workdir = tempfile.mkdtemp(prefix="brood-deploy.")
        try:
            self._overlay(workdir, digests, server, admin, cliproxy, tls, api_rules)
            self._kubectl("delete", *LEGACY, "-n", NAMESPACE, "--ignore-not-found")
            self._kubectl("apply", "--server-side", "--force-conflicts", "-k", workdir)
            self._bootstrap_pool(workdir, worker_image)
        finally:
            shutil.rmtree(workdir, ignore_errors=True)
        # Контроллер создаёт Deployment с именем WorkerPool (workerpool_apply.go).
        self._kubectl("wait", "--for=create", "deployment/brood", "-n", NAMESPACE,
                      "--timeout=600s")
        self._kubectl("rollout", "status", "deployment/brood", "-n", NAMESPACE,
                      "--timeout=600s")
        for what in ROLLOUT:
            self._kubectl("rollout", "status", what, "-n", NAMESPACE, "--timeout=600s")
        port = local.edge_ports(self.get).get(dict(local.EDGE)["BROOD_HTTPS_PORT"])
        if not port:
            return digests, None
        host = first_host(server["DJANGO_ALLOWED_HOSTS"])
        self._edge(port, host, tls["tls.crt"], cliproxy["CLIPROXY_CLIENT_KEY"])
        return digests, f"https://{host}" + ("" if port == 443 else f":{port}") + "/"

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
        code, out, err = self.shell.output(
            "kubectl", f"--context={local.CONTEXT}", "get", "namespace", NAMESPACE,
            "--ignore-not-found", "-o", "json", "--request-timeout=15s")
        if code:
            raise RuntimeError(f"cannot inspect namespace {NAMESPACE}: {local.reason(err, code)}")
        if not out.strip():
            return
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
        """Тот же pinned ko, .ko.yaml, version stamp и host platform, что ate-setup.

        Go сохраняет кэш компиляции, KO_CACHE -- слои между релизами brood.
        Своего кэша по git HEAD нет: он пропустил бы локальные правки source."""
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
        # ate-setup/internal/ko/ko.go: VERSION, затем git describe, затем dev.
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

    def _bootstrap_pool(self, workdir, image):
        path = os.path.join(workdir, "initial-pool.json")
        with open(path, "w", encoding="utf-8") as f:
            json.dump(initial_pool(image), f)
        code, _, err = self.shell.output("kubectl", f"--context={local.CONTEXT}",
                                          "create", "-f", path)
        # create атомарен: гонка с панелью не повод менять уже существующий пул.
        if code and not re.search(r'\(AlreadyExists\): (?:error when creating "[^"\n]+": )?'
                                  r'workerpools(?:\.ate\.dev)? "brood" already exists\s*$', err):
            raise RuntimeError(f"create WorkerPool brood/brood: {local.reason(err, code)}")

    def _image(self, name, dockerfile):
        """Собрать и запушить образ -> digest. Тег -- только имя, под которым
        пушить: в манифест едет digest, и устаревший тег не разрешится."""
        tag = f"{REGISTRY}/{name}:build-{int(time.time())}"
        self.shell.run("docker", "buildx", "build", "--push", "-f", dockerfile, "-t", tag,
                       self.project)
        code, out, err = self.shell.output("docker", "buildx", "imagetools", "inspect", tag,
                                           "--format", "{{json .Manifest}}")
        if code:
            raise RuntimeError(f"docker buildx imagetools inspect {tag}: {local.reason(err, code)}")
        digest = json.loads(out).get("digest", "")
        if not digest.startswith("sha256:"):
            raise RuntimeError(f"no digest for {tag} in the registry")
        return digest

    def _overlay(self, workdir, digests, server, admin, cliproxy, tls, api_rules):
        os.chmod(workdir, 0o700)
        shutil.copytree(MANIFESTS, os.path.join(workdir, "base"))
        policy = {"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
                  "metadata": {"name": "django-kube-api", "namespace": NAMESPACE},
                  "spec": {"podSelector": {"matchLabels": {"app": "django"}},
                           "policyTypes": ["Egress"], "egress": api_rules}}
        files = {"kustomization.yaml": kustomization(digests),
                 "api-egress.json": json.dumps(policy),
                 "server.env": env_file(server), "admin.env": env_file(admin),
                 "cliproxy.env": env_file(cliproxy), **tls}
        for name, text in files.items():
            fd = os.open(os.path.join(workdir, name), os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with os.fdopen(fd, "w", encoding="utf-8") as f:
                f.write(text)

    def _edge(self, port, host, cert, client_key):
        """Вход стенда отвечает по https на порту хоста ровно выложенным
        сертификатом, и каждый его маршрут доходит до своего соседа: панель,
        Django, ворота management без сессии и LLM API прокси с клиентским
        ключом. NodePort доезжает до kube-proxy не мгновенно -- поэтому с
        повтором."""
        checks = ((("GET", "/", {}), 200), (("GET", "/admin/login/", {}), 200),
                  (("GET", "/v8/management/config", {}), 401),
                  (("GET", "/v1/models", {"Authorization": f"Bearer {client_key}"}), 200))
        for (method, path, headers), want in checks:
            self._answers(port, host, cert, method, path, headers, want)

    def _answers(self, port, host, cert, method, path, headers, want):
        expected = ssl.PEM_cert_to_DER_cert(cert)
        context = ssl.create_default_context()
        context.check_hostname = False
        context.verify_mode = ssl.CERT_NONE
        deadline = self.clock() + EDGE_DEADLINE
        while True:
            try:
                conn = http.client.HTTPSConnection("127.0.0.1", port, context=context, timeout=5)
                try:
                    conn.request(method, path, headers={"Host": host, **headers})
                    # Сертификат -- до ответа: на «Connection: close» (старый
                    # под edge, пока nginx плавно останавливается) http.client
                    # отдаёт сокет ответу и обнуляет conn.sock.
                    served = conn.sock.getpeercert(binary_form=True)
                    status = conn.getresponse().status
                finally:
                    conn.close()
                if served != expected:
                    raise RuntimeError(f"https on port {port} serves another certificate than "
                                       f"{TLS['tls.crt']}")
                if status == want:
                    return
                last = f"HTTP {status}, want {want}"
            except OSError as e:
                last = str(e)
            if self.clock() >= deadline:
                raise RuntimeError(f"{method} https://{host}{path} via 127.0.0.1:{port} did not "
                                   f"answer within {EDGE_DEADLINE} seconds: {last}")
            self.sleep(2)

    def _kubectl(self, *args):
        self.shell.run("kubectl", f"--context={local.CONTEXT}", *args)
