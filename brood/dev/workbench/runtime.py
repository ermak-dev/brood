"""Подготовка файлов и собственного Docker внутри development microVM."""
import json
import os
import secrets
import shutil
import stat
import subprocess
import time
from pathlib import Path

from brood.dev.deploy.server import env_file


SOCKET = "unix:///var/run/docker.sock"


def check_environment(env):
    if env.get("BROOD_WORKBENCH") != "1":
        raise RuntimeError("workbench start is only available inside the workbench image")
    if env.get("DOCKER_HOST") != SOCKET:
        raise RuntimeError(f"DOCKER_HOST must be {SOCKET}; a host Docker daemon is not allowed")


def development_settings(root, token):
    """Новый тестовый стенд не получает ни одного секрета рабочей установки."""
    return {
        "DJANGO_SECRET_KEY": token(), "POSTGRES_PASSWORD": token(),
        "DJANGO_SUPERUSER_PASSWORD": token(), "CLIPROXY_MANAGEMENT_KEY": token(),
        "CLIPROXY_CLIENT_KEY": token(), "DJANGO_SUPERUSER_USERNAME": "developer",
        "DJANGO_ALLOWED_HOSTS": "brood,localhost,127.0.0.1", "DJANGO_DEBUG": "0",
        "BROOD_HTTP_PORT": "8080", "BROOD_HTTPS_PORT": "8443",
        "SUBSTRATE_SOURCE": str(Path(root) / "substrate"),
    }


def checked_run(args, run=subprocess.run):
    try:
        result = run(args, capture_output=True, text=True)
    except FileNotFoundError:
        raise RuntimeError(f"{args[0]} is not installed") from None
    if result.returncode:
        raise RuntimeError(f"{args[0]} failed: {(result.stderr or result.stdout).strip()}")
    return result.stdout.strip()


def prepare_workspace(root, seeds, token=secrets.token_urlsafe, run=subprocess.run):
    """Скопировать начальные деревья, не заменяя уже существующую работу."""
    root, seeds = Path(root), Path(seeds)
    if not root.is_absolute() or root == Path("/") or not seeds.is_absolute():
        raise RuntimeError("workbench root and seeds must be separate absolute directories")
    if root == seeds or root in seeds.parents or seeds in root.parents:
        raise RuntimeError("workbench root and seeds must be separate directories")
    if (seeds / "brood/.env").exists():
        raise RuntimeError("workbench seed must not contain .env")
    for name, marker in (("brood", "bin/brood"), ("substrate", "hack/install-ate-kind.sh")):
        if not (seeds / name / marker).is_file():
            raise RuntimeError(f"workbench seed {name} has no {marker}")
    root.mkdir(parents=True, exist_ok=True)
    for name in ("home", "docker", "tmp", "data", "logs", "results"):
        (root / name).mkdir(exist_ok=True)
    for name in ("brood", "substrate"):
        target = root / name
        if not target.exists():
            shutil.copytree(seeds / name, target, symlinks=True)
    project = root / "brood"
    env = project / ".env"
    if not env.exists():
        text = env_file(development_settings(root, token))
        fd = os.open(env, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            f.write(text)
    tls = project / "secrets"
    tls.mkdir(exist_ok=True)
    key, cert = tls / "tls.key", tls / "tls.crt"
    if key.exists() != cert.exists():
        raise RuntimeError("workbench TLS pair is incomplete; refusing to overwrite it")
    if not key.exists():
        checked_run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes",
                     "-days", "7", "-subj", "/CN=brood", "-addext",
                     "subjectAltName=DNS:brood,DNS:localhost,IP:127.0.0.1",
                     "-keyout", str(key), "-out", str(cert)], run)
        key.chmod(0o600)
        cert.chmod(0o600)
    return project


def prepare_kernel_log_device(stat_file=os.stat, make_node=os.mknod,
                              open_device=os.open, close_device=os.close):
    """Kubelet читает настоящий kernel log гостя, не файл и не /dev/null."""
    path, device = "/dev/kmsg", os.makedev(1, 11)
    try:
        try:
            entry = stat_file(path, follow_symlinks=False)
        except FileNotFoundError:
            make_node(path, stat.S_IFCHR | 0o600, device)
            entry = stat_file(path, follow_symlinks=False)
        if not stat.S_ISCHR(entry.st_mode) or entry.st_rdev != device:
            raise RuntimeError("workbench /dev/kmsg must be the guest kernel device c 1:11")
        fd = open_device(path, os.O_RDONLY | os.O_NONBLOCK)
        close_device(fd)
    except OSError as error:
        raise RuntimeError(f"workbench guest /dev/kmsg is unavailable: {error}") from None


def daemon_command(root):
    root = Path(root)
    return ["dind", "dockerd", f"--host={SOCKET}", f"--data-root={root / 'docker'}",
            f"--pidfile={root / 'docker.pid'}", "--storage-driver=vfs",
            "--feature=containerd-snapshotter=false"]


def write_status(root, state, message):
    directory = Path(root) / "results"
    directory.mkdir(parents=True, exist_ok=True)
    path = directory / "status.json"
    temporary = directory / "status.tmp"
    temporary.write_text(json.dumps({"state": state, "message": message}) + "\n")
    temporary.chmod(0o644)
    temporary.replace(path)


def start_docker(root, run=subprocess.run, popen=subprocess.Popen,
                 clock=time.monotonic, sleep=time.sleep):
    """Daemon принадлежит этому контейнеру; ошибка не трогает чужой процесс."""
    if not os.path.ismount("/sys/fs/cgroup"):
        checked_run(["mount", "-t", "cgroup2", "none", "/sys/fs/cgroup"], run)
    checked_run(["mount", "--make-rshared", "/"], run)
    log = Path(root) / "logs/dockerd.log"
    try:
        with log.open("ab", buffering=0) as output:
            process = popen(daemon_command(root), stdout=output, stderr=subprocess.STDOUT)
    except FileNotFoundError:
        raise RuntimeError("dockerd is not installed in the workbench image") from None
    deadline = clock() + 90
    while True:
        if process.poll() is not None:
            raise RuntimeError(f"workbench Docker exited; read {log}")
        result = run(["docker", "info", "--format", "{{.ID}}"], capture_output=True, text=True)
        if result.returncode == 0:
            return process
        if clock() >= deadline:
            process.terminate()
            process.wait(timeout=15)
            raise RuntimeError(f"workbench Docker did not become ready; read {log}")
        sleep(1)
