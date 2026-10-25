"""Локальный Substrate: кластер kind `substrate` и реестр образов рядом с ним.

Порт hack/substrate.sh. Substrate собирается из checkout'а substrate и
раскатывается в отдельный кластер kind со своим kubeconfig: текущий контекст
kubectl не меняется. Checkout -- SUBSTRATE_SOURCE разработчика либо свой
клон официального репозитория на закреплённом коммите (sync). Состояние лежит в
<checkout>/bin/substrate, там же, где его держал скрипт: kubectl, kubeconfig
и записи владения. Установка, сделанная скриптом, остаётся той же установкой.

Записи владения -- метка `owned` и Id обоих контейнеров. Имена контейнеров
постоянные, и без записей команда остановила бы или удалила чужой кластер
с тем же именем: каждая команда сверяет их до первой правки.

Модуль ничего не печатает: процессы запускает Shell, которую передаёт
командлет, и вывод процессов идёт туда, куда она его направит.
"""
import hashlib
import json
import os
import shutil
import subprocess
import tempfile
import time
import urllib.error
import urllib.request

CLUSTER = "substrate"
CONTEXT = f"kind-{CLUSTER}"
NODE = f"{CLUSTER}-control-plane"
REGISTRY = "substrate-registry"
# Метка, которой реестр помечен при создании: по ней чужой контейнер с тем
# же именем отличается от своего.
REGISTRY_OWNER = "substrate-local"
REGISTRY_PORT = 5002
REGISTRY_IMAGE = ("registry:3@sha256:"
                  "01cd312e67026f06f5d89c3e133a4a9adc7cb9eeb9b1194e0b2954dfd770efd2")
# containerd узла тянет localhost:5002 из реестра по имени в сети kind.
HOSTS_DIR = f"/etc/containerd/certs.d/localhost:{REGISTRY_PORT}"
HOSTS_TOML = f'[host."http://{REGISTRY}:5000"]\n'
NAMESPACE = "ate-system"
KUBECTL_VERSION = "v1.37.1"
KIND_CONFIG = os.path.join(os.path.dirname(os.path.realpath(__file__)), "kind.yaml")
# Входы стенда: настройка с портом хоста -> NodePort Service brood-server
# (brood/dev/deploy/manifests/server.yaml). kind публикует порты узла только
# при создании кластера, поэтому они -- часть установки, а не выкладки.
EDGE = (("BROOD_HTTP_PORT", 30080), ("BROOD_HTTPS_PORT", 30443))
API_DEADLINE = 300
# docker проверяет `docker info` до всего остального; go собирает Substrate.
TOOLS = ("go",)
DEPLOY = ("deploy", NAMESPACE, '--credential-provider={"name":"k8s.io"}',
          f"--ingress-allowed-spiffe-ids=spiffe://cluster.local/ns/{NAMESPACE}/sa/ate-client",
          "--rollout-timeout=10m", f"--namespace={NAMESPACE}", "--atenet-dataplane=envoy",
          # Под django brood -- глобальный владелец ateapi: проекты панели
          # создают и удаляют atespace. Общий ate-client не расшариваем:
          # его токен заминтовал бы любой с RBAC на его ServiceAccount.
          "--ateapi-authz-bootstrap-owners="
          f"system:serviceaccount:brood:django,system:serviceaccount:{NAMESPACE}:ate-client")
# По ним checkout substrate отличается от любого другого каталога.
MARKERS = ("hack/kind.sh", "hack/install-ate-kind.sh", "cmd/kubectl-ate")
# Что ставит install без SUBSTRATE_SOURCE. Коммит закреплён, как образ реестра
# по digest: раскатка одного коммита brood всегда ставит один и тот же
# Substrate. Новый Substrate -- правка этой строки, после того как DEPLOY
# сверен с его hack/install-ate-kind.sh.
REPOSITORY = "https://github.com/agent-substrate/substrate.git"
COMMIT = "4ebd668d62e3f55753765a46579fc6deb81b2f7f"


def atelet_registry_patch(daemonset):
    """Kind overlay знает свой реестр, а containerd и atelet должны знать наш."""
    try:
        metadata = daemonset["metadata"]
        if not metadata.get("uid") or not metadata.get("resourceVersion"):
            raise ValueError("missing identity")
        containers = daemonset["spec"]["template"]["spec"]["containers"]
        matches = [(i, container) for i, container in enumerate(containers)
                   if container["name"] == "atelet"]
        if len(matches) != 1:
            raise ValueError("expected one atelet container")
        index, container = matches[0]
        args = container["args"]
        flag = "--localhost-registry-replacement="
        positions = [i for i, arg in enumerate(args) if arg.startswith(flag)]
        if len(positions) != 1:
            raise ValueError("expected one localhost registry replacement flag")
        position = positions[0]
        value = f"{flag}{REGISTRY}:5000"
        if args[position] == value:
            return []
        path = f"/spec/template/spec/containers/{index}/args"
        return [{"op": "test", "path": f"/metadata/{key}", "value": metadata[key]}
                for key in ("uid", "resourceVersion")] + [
                    {"op": "test", "path": path, "value": args},
                    {"op": "replace", "path": f"{path}/{position}", "value": value}]
    except (KeyError, TypeError, ValueError, AttributeError) as e:
        raise RuntimeError(f"cannot configure atelet actor registry: {e}") from None


def managed_checkout(environ):
    """Каталог своего клона -> путь. Чистая функция.

    Каталог данных пользователя ($XDG_DATA_HOME, без него ~/.local/share), а
    не рабочая копия brood: у CI она своя на каждую джобу, а записи владения
    кластера в <клон>/bin/substrate обязаны пережить джобу."""
    data = environ.get("XDG_DATA_HOME", "")
    if not os.path.isabs(data):
        data = os.path.join(environ.get("HOME") or os.path.expanduser("~"), ".local", "share")
    return os.path.join(data, "brood", "substrate")


def sync(shell, path, repository=REPOSITORY, commit=COMMIT):
    """Клон repository в path на коммите commit -> path.

    Нет клона -- заводится; клон уже на commit -- сеть не нужна; иначе
    fetch одного коммита и переход на него. Неотслеживаемое (bin/substrate с
    записями владения) переживает переход. Отказ до правок: каталог не клон
    и не пуст, origin другой, в отслеживаемых файлах правки -- это чужое."""
    git = ("git", "-C", path)
    if not os.path.isdir(os.path.join(path, ".git")):
        if os.path.isdir(path) and os.listdir(path):
            raise RuntimeError(f"{path} exists and is not a git checkout; refusing to use it")
        os.makedirs(path, exist_ok=True)
        shell.run("git", "init", "-q", path)
        shell.run(*git, "remote", "add", "origin", repository)
    code, out, err = shell.output(*git, "remote", "get-url", "origin")
    if code:
        raise RuntimeError(f"git remote get-url in {path}: {reason(err, code)}")
    if out.strip() != repository:
        raise RuntimeError(f"{path}: origin is {out.strip()}, not {repository}; "
                           f"refusing to change it")
    code, out, _ = shell.output(*git, "rev-parse", "--verify", "-q", "HEAD")
    if code == 0 and out.strip() == commit:
        return path
    code, out, err = shell.output(*git, "status", "--porcelain", "--untracked-files=no")
    if code or out.strip():
        raise RuntimeError(f"{path} has local changes; refusing to check out {commit[:12]}")
    shell.run(*git, "fetch", "-q", "--depth", "1", "origin", commit)
    shell.run(*git, "checkout", "-q", "--detach", commit)
    return path


def checkout(path):
    """Каталог checkout'а substrate -> абсолютный путь. Не checkout -- отказ с
    первым недостающим файлом: иначе команда упала бы позже, посреди
    установки, на чужом пути."""
    path = os.path.abspath(path)
    for marker in MARKERS:
        if not os.path.exists(os.path.join(path, marker)):
            raise RuntimeError(f"{path} is not a Substrate checkout: no {marker}")
    return path


def state_dir(source):
    return os.path.join(source, "bin", "substrate")


def environment(base, source):
    """Окружение процессов установки. Чистая функция.

    kubectl и kubeconfig -- свои, из state: первым в PATH, KUBECONFIG -- его
    файл. ATE_CONFIG снят: установщик прочёл бы чужой конфиг вместо флагов."""
    state = state_dir(source)
    env = {k: v for k, v in base.items() if k != "ATE_CONFIG"}
    path = [state, os.path.join(source, "bin")] + ([base["PATH"]] if base.get("PATH") else [])
    env.update(PATH=os.pathsep.join(path),
               KUBECONFIG=os.path.join(state, "kubeconfig"),
               KIND_CLUSTER_NAME=CLUSTER,
               KUBECTL_CONTEXT=CONTEXT,
               KO_DOCKER_REPO=f"localhost:{REGISTRY_PORT}")
    return env


def edge_ports(get):
    """Настройки (get -- config.get) -> {порт узла: порт хоста}. Чистая
    функция. Не задана -- порта нет: на машине разработчика сервер достают
    port-forward'ом."""
    out = {}
    for name, node_port in EDGE:
        value = get(name)
        if not value:
            continue
        if not value.isdigit() or not 0 < int(value) < 65536:
            raise RuntimeError(f"{name} in .env must be a TCP port, got {value!r}")
        out[node_port] = int(value)
    return out


def kind_config(text, ports):
    """Текст kind.yaml -> он же с extraPortMappings узла. Чистая функция."""
    if not ports:
        return text
    anchor = "- role: control-plane\n"
    if anchor not in text:
        raise RuntimeError(f"{KIND_CONFIG} has no control-plane node to publish ports on")
    mappings = "".join(f"  - containerPort: {node}\n    hostPort: {host}\n"
                       for node, host in sorted(ports.items()))
    return text.replace(anchor, anchor + "  extraPortMappings:\n" + mappings, 1)


def published(bindings):
    """PortBindings узла из docker inspect -> {порт узла: порт хоста} входов
    стенда. Чистая функция: прочие порты (API-сервер) не входы."""
    edge = {node for _, node in EDGE}
    out = {}
    for port, binds in (bindings or {}).items():
        node = int(port.split("/")[0])
        if node in edge and binds:
            out[node] = int(binds[0]["HostPort"])
    return out


def reason(err, code):
    """Причина отказа процесса: первая непустая строка stderr либо код."""
    return next((line.strip() for line in (err or "").splitlines() if line.strip()),
                f"exit {code}")


def step(argv):
    """Как назвать процесс в отказе: имя программы и два первых аргумента.
    `-C <каталог>` git пропускается: шаг -- подкоманда, а не каталог."""
    words = list(argv[1:])
    if words[:1] == ["-C"]:
        words = words[2:]
    return " ".join([os.path.basename(argv[0]), *words[:2]])


class Shell:
    """Процессы установки с общим окружением и каталогом. Вывод run идёт в
    унаследованные stdout и stderr, output его захватывает."""

    def __init__(self, env, cwd):
        self.env, self.cwd = env, cwd

    def run(self, *argv, stdin=None):
        """Процесс с выводом наружу. Ненулевой код -- отказ с именем шага: свой
        вывод процесс уже напечатал."""
        try:
            r = subprocess.run(argv, env=self.env, cwd=self.cwd, input=stdin, text=True)
        except FileNotFoundError:
            raise RuntimeError(f"{argv[0]} is not installed") from None
        if r.returncode:
            raise RuntimeError(f"{step(argv)} exited with {r.returncode}")

    def output(self, *argv):
        """Процесс с захватом вывода -> (код, stdout, stderr)."""
        try:
            r = subprocess.run(argv, env=self.env, cwd=self.cwd, capture_output=True, text=True)
        except FileNotFoundError:
            raise RuntimeError(f"{argv[0]} is not installed") from None
        return r.returncode, r.stdout, r.stderr

    def which(self, tool):
        return shutil.which(tool, path=self.env.get("PATH")) is not None


def fetch(url, path, attempts=4):
    """Скачать url в файл path. Сбой сети повторяется, ответ HTTP с ошибкой
    -- нет: повтор его не исправит."""
    for attempt in range(attempts):
        try:
            with urllib.request.urlopen(url, timeout=60) as r, open(path, "wb") as f:
                shutil.copyfileobj(r, f)
            return
        except urllib.error.HTTPError as e:
            raise RuntimeError(f"GET {url} -> HTTP {e.code}") from None
        except (urllib.error.URLError, TimeoutError, ConnectionError) as e:
            last = getattr(e, "reason", e)
            if attempt + 1 < attempts:
                time.sleep(2 ** attempt)
    raise RuntimeError(f"GET {url}: {last}")


class LocalCluster:
    """Установка Substrate в кластер kind из checkout'а source. Процессы --
    через shell; скачивание, ожидание и часы передаются, чтобы проверки не
    ходили в сеть и не спали."""

    def __init__(self, source, shell, fetch=fetch, sleep=time.sleep, clock=time.monotonic,
                 ports=None):
        self.source, self.shell = source, shell
        self.ports = ports or {}
        self.fetch, self.sleep, self.clock = fetch, sleep, clock
        self.state = state_dir(source)
        self.kubeconfig = os.path.join(self.state, "kubeconfig")
        self.kubectl = os.path.join(self.state, "kubectl")
        self.kubectl_ate = os.path.join(source, "bin", "kubectl-ate")
        self.owned = os.path.join(self.state, "owned")
        self.ids = {NODE: os.path.join(self.state, "node-id"),
                    REGISTRY: os.path.join(self.state, "registry-id")}

    # ─── команды ─────────────────────────────────────────────────────────
    def install(self):
        """Собрать Substrate из checkout'а и раскатать в кластер; повторный
        вызов раскатывает изменения в тот же кластер."""
        self._docker()
        missing = [tool for tool in TOOLS if not self.shell.which(tool)]
        if missing:
            raise RuntimeError(f"brood substrate install needs {', '.join(missing)} on PATH")
        if not os.path.isfile(self.owned):
            taken = [name for name in (NODE, REGISTRY) if self._exists(name)]
            if taken:
                raise RuntimeError(f"container {', '.join(taken)} already exists and this "
                                   f"installation does not own it; refusing to replace it")
            os.makedirs(self.state, exist_ok=True)
            open(self.owned, "a").close()
        self._check_ownership()
        if not os.access(self.kubectl, os.X_OK):
            self._download_kubectl()
        run = self.shell.run
        if not self._exists(REGISTRY):
            run("docker", "run", "-d", "--restart=no", "--label", f"created-by={REGISTRY_OWNER}",
                "-p", f"127.0.0.1:{REGISTRY_PORT}:5000", "--name", REGISTRY, REGISTRY_IMAGE)
            self._record(REGISTRY)
        else:
            run("docker", "start", REGISTRY)
        if not self._exists(NODE):
            config_path = KIND_CONFIG
            if self.ports:
                with open(KIND_CONFIG, encoding="utf-8") as f:
                    rendered = kind_config(f.read(), self.ports)
                config_path = os.path.join(self.state, "kind.yaml")
                with open(config_path, "w", encoding="utf-8") as f:
                    f.write(rendered)
            self._kind("create", "cluster", "--name", CLUSTER, "--config", config_path,
                       "--wait", "180s")
            self._record(NODE)
        else:
            self._check_ports()
            run("docker", "start", NODE)
        if self._inspect("{{json .NetworkSettings.Networks.kind}}", REGISTRY) == "null":
            run("docker", "network", "connect", "kind", REGISTRY)
        self._require_ipv6()
        self._proxy_arp()
        run("docker", "exec", NODE, "mkdir", "-p", HOSTS_DIR)
        run("docker", "exec", "-i", NODE, "cp", "/dev/stdin", f"{HOSTS_DIR}/hosts.toml",
            stdin=HOSTS_TOML)
        self._kind("export", "kubeconfig", "--name", CLUSTER)
        self._wait_for_api()
        run(os.path.join(self.source, "hack", "install-ate-kind.sh"), *DEPLOY)
        self._actor_registry()
        run("go", "build", "-o", self.kubectl_ate, "./cmd/kubectl-ate")
        self._start()

    def start(self):
        """Поднять остановленную установку и дождаться, пока Substrate готов."""
        self._docker()
        self._start()

    def stop(self):
        """Остановить контейнеры; данные кластера остаются."""
        self._docker()
        self._check_ownership()
        for name in (NODE, REGISTRY):
            if self._exists(name):
                self.shell.run("docker", "stop", name)

    def uninstall(self):
        """Удалить кластер, реестр и записи владения; данные кластера -- тоже."""
        self._docker()
        self._check_ownership()
        self._kind("delete", "cluster", "--name", CLUSTER)
        if self._exists(REGISTRY):
            self.shell.run("docker", "rm", "-f", REGISTRY)
        for path in (self.owned, *self.ids.values(), self.kubeconfig):
            try:
                os.remove(path)
            except FileNotFoundError:
                pass

    def check(self):
        """Сверка владения для соседних команд (brood deploy): отказ до их
        первой правки в кластере."""
        self._docker()
        self._check_ownership()
        if not self._exists(NODE):
            raise RuntimeError("Substrate is incomplete: run brood substrate install")

    # ─── шаги ────────────────────────────────────────────────────────────
    def _check_ports(self):
        """Порты узла сходятся с настройками -- иначе отказ: kind задаёт их
        только при создании, а пересоздание стирает кластер, и решать это
        человеку, а не установке."""
        have = published(json.loads(self._inspect("{{json .HostConfig.PortBindings}}", NODE)))
        if have != self.ports:
            def show(ports):
                return ", ".join(f"{h}->{n}" for n, h in sorted(ports.items())) or "none"
            raise RuntimeError(f"{NODE} publishes host ports {show(have)}, .env wants "
                               f"{show(self.ports)}: kind sets them only when it creates the "
                               f"cluster; recreate it with brood substrate uninstall, then "
                               f"brood substrate install")

    def _start(self):
        self._check_ownership()
        if not (self._exists(NODE) and self._exists(REGISTRY)):
            raise RuntimeError("Substrate is incomplete: run brood substrate install")
        self.shell.run("docker", "start", REGISTRY)
        self.shell.run("docker", "start", NODE)
        self._require_ipv6()
        self._proxy_arp()
        self._wait_for_api()
        self._kubectl("wait", "--for=condition=Ready", "node", "--all", "--timeout=300s")
        self._kubectl("wait", "--for=condition=Available", "deployment", "--all",
                      "-n", NAMESPACE, "--timeout=600s")
        self._kubectl("rollout", "status", "daemonset/atelet", "-n", NAMESPACE, "--timeout=600s")

    def _docker(self):
        code, _, err = self.shell.output("docker", "info")
        if code:
            raise RuntimeError(f"docker is not available: {reason(err, code)}")

    def _check_ownership(self):
        """Записи владения сходятся с контейнерами -- иначе отказ до правок."""
        if not os.path.isfile(self.owned):
            raise RuntimeError("Substrate is not installed: run brood substrate install")
        if self._exists(NODE) and \
                self._inspect('{{index .Config.Labels "io.x-k8s.kind.cluster"}}', NODE) != CLUSTER:
            raise RuntimeError(f"refusing to modify {NODE}: it is not a node of kind cluster "
                               f"{CLUSTER}")
        if self._exists(REGISTRY) and \
                self._inspect('{{index .Config.Labels "created-by"}}', REGISTRY) != REGISTRY_OWNER:
            raise RuntimeError(f"refusing to modify {REGISTRY}: it was not created by "
                               f"{REGISTRY_OWNER}")
        for name, path in self.ids.items():
            record = _read(path)
            if self._exists(name) and (not record or self._inspect("{{.Id}}", name) != record):
                raise RuntimeError(f"container {name} has no matching ownership record "
                                   f"in {path}; refusing to modify it")

    def _require_ipv6(self):
        """Envoy Substrate слушает dual-stack и без IPv6 в ядре Docker не
        поднимается, даже в кластере IPv4."""
        code, _, err = self.shell.output("docker", "exec", NODE, "test", "-d", "/proc/sys/net/ipv6")
        if code == 1:
            raise RuntimeError("Substrate Envoy needs IPv6 in the Docker kernel, even for an "
                               "IPv4 cluster: on Linux, boot without ipv6.disable=1 and run "
                               "the command again")
        if code:
            raise RuntimeError(f"docker exec {NODE}: {reason(err, code)}")

    def _actor_registry(self):
        code, out, err = self.shell.output(
            "kubectl", f"--context={CONTEXT}", "get", "daemonset/atelet", "-n", NAMESPACE,
            "-o", "json", "--request-timeout=30s")
        if code:
            raise RuntimeError(f"cannot inspect atelet actor registry: {reason(err, code)}")
        try:
            daemonset = json.loads(out)
        except ValueError:
            raise RuntimeError("cannot inspect atelet actor registry: invalid JSON") from None
        patch = atelet_registry_patch(daemonset)
        if patch:
            self._kubectl("patch", "daemonset/atelet", "-n", NAMESPACE, "--type=json",
                          "-p", json.dumps(patch), "--request-timeout=30s")

    def _proxy_arp(self):
        self.shell.run("docker", "exec", NODE, "sysctl", "net.ipv4.conf.all.proxy_arp=1")

    def _wait_for_api(self):
        deadline = self.clock() + API_DEADLINE
        while self.shell.output("kubectl", f"--context={CONTEXT}", "get", "--raw=/readyz",
                                "--request-timeout=5s")[0]:
            if self.clock() >= deadline:
                raise RuntimeError(f"Kubernetes API did not become ready within {API_DEADLINE} "
                                   f"seconds")
            self.sleep(1)

    def _download_kubectl(self):
        """kubectl той версии, под которую написана установка, со сверкой
        контрольной суммы; на место -- только сверенный."""
        goos, goarch = (self._go_env(name) for name in ("GOOS", "GOARCH"))
        url = f"https://dl.k8s.io/release/{KUBECTL_VERSION}/bin/{goos}/{goarch}/kubectl"
        download = tempfile.mkdtemp(prefix="download.", dir=self.state)
        try:
            binary = os.path.join(download, "kubectl")
            self.fetch(url, binary)
            self.fetch(url + ".sha256", binary + ".sha256")
            want = _read(binary + ".sha256")
            got = _sha256(binary)
            if got != want:
                raise RuntimeError(f"kubectl {KUBECTL_VERSION} checksum mismatch: "
                                   f"got {got}, want {want}")
            os.chmod(binary, 0o755)
            os.replace(binary, self.kubectl)
        finally:
            shutil.rmtree(download, ignore_errors=True)

    def _go_env(self, name):
        code, out, err = self.shell.output("go", "env", name)
        if code:
            raise RuntimeError(f"go env {name}: {reason(err, code)}")
        return out.strip()

    def _kind(self, *args):
        self.shell.run(os.path.join(self.source, "hack", "kind.sh"), *args)

    def _kubectl(self, *args):
        self.shell.run("kubectl", f"--context={CONTEXT}", *args)

    def _exists(self, name):
        return self.shell.output("docker", "container", "inspect", name)[0] == 0

    def _inspect(self, fmt, name):
        code, out, err = self.shell.output("docker", "inspect", "-f", fmt, name)
        if code:
            raise RuntimeError(f"docker inspect {name}: {reason(err, code)}")
        return out.strip()

    def _record(self, name):
        """Id только что созданного контейнера -- в его запись владения."""
        with open(self.ids[name], "w") as f:
            f.write(self._inspect("{{.Id}}", name) + "\n")


def _read(path):
    """Файл без пробелов по краям; нет файла -- пусто."""
    try:
        with open(path, encoding="utf-8") as f:
            return f.read().strip()
    except FileNotFoundError:
        return ""


def _sha256(path):
    digest = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()
