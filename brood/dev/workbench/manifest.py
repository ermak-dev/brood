"""Чистые манифесты изолированного окружения разработки Brood."""

import ipaddress
import re


PUBLIC_HOSTS = (
    "pypi.org", "files.pythonhosted.org", "registry.npmjs.org", "index.crates.io", "static.crates.io",
    "gcr.io", "github.com", "api.github.com",
    "raw.githubusercontent.com", "codeload.github.com",
    "release-assets.githubusercontent.com", "objects.githubusercontent.com",
    "pkg-containers.githubusercontent.com", "ghcr.io", "proxy.golang.org",
    "sum.golang.org", "go.dev", "dl.google.com", "storage.googleapis.com",
    "dl.k8s.io", "cdn.dl.k8s.io", "registry.k8s.io", "auth.docker.io",
    "us-docker.pkg.dev", "us-central1-docker.pkg.dev", "us-west1-docker.pkg.dev",
    "us-west2-docker.pkg.dev", "europe-west1-docker.pkg.dev", "europe-west2-docker.pkg.dev",
    "asia-east1-docker.pkg.dev",
    "registry-1.docker.io", "production.cloudflare.docker.com", "production.cloudfront.docker.com", "cgr.dev",
    "docker-images-prod.6aa30f8b08e16409b46e0173d6de2f56.r2.cloudflarestorage.com",
)

CLAUDE_HOSTS = (
    "api.anthropic.com", "claude.ai", "claude.com", "platform.claude.com",
    "sso.claude.com", "downloads.claude.ai", "code.claude.com",
)
_DEFAULT_NAME = object()


# Полный набор гостя нужен вложенному Docker; границей изоляции остаётся ядро VM.
# Доступ к устройствам хоста этим списком не расширяется.
GUEST_CAPABILITIES = (
    "CHOWN", "DAC_OVERRIDE", "DAC_READ_SEARCH", "FOWNER", "FSETID", "KILL",
    "SETGID", "SETUID", "SETPCAP", "LINUX_IMMUTABLE", "NET_BIND_SERVICE",
    "NET_BROADCAST", "NET_ADMIN", "NET_RAW", "IPC_LOCK", "IPC_OWNER", "SYS_MODULE",
    "SYS_RAWIO", "SYS_CHROOT", "SYS_PTRACE", "SYS_PACCT", "SYS_ADMIN", "SYS_BOOT",
    "SYS_NICE", "SYS_RESOURCE", "SYS_TIME", "SYS_TTY_CONFIG", "MKNOD", "LEASE",
    "AUDIT_WRITE", "AUDIT_CONTROL", "SETFCAP", "MAC_OVERRIDE", "MAC_ADMIN", "SYSLOG",
    "WAKE_ALARM", "BLOCK_SUSPEND", "AUDIT_READ", "PERFMON", "BPF", "CHECKPOINT_RESTORE",
)


def _dns_label(value):
    if not isinstance(value, str) or not re.fullmatch(
            r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?", value):
        raise ValueError("name must be a lowercase DNS label of 1 to 63 characters")


def _image(value):
    component = r"[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*"
    registry = r"[a-z0-9]+(?:[.-][a-z0-9]+)*(?::[0-9]+)?"
    reference = (rf"(?:{registry}/)?{component}(?:/{component})*"
                 r"(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?@sha256:[0-9a-f]{64}")
    if not isinstance(value, str) or len(value) > 512 or not re.fullmatch(reference, value):
        raise ValueError("image must be a valid OCI reference pinned by sha256 digest")


def _hostname(value):
    if not isinstance(value, str) or len(value) > 253 or "." not in value:
        raise ValueError("egress hostname must be an exact public DNS hostname")
    try:
        ipaddress.ip_address(value)
    except ValueError:
        pass
    else:
        raise ValueError("egress hostname must not be an IP address")
    if value.endswith((".local", ".localhost", ".internal", ".invalid", ".test")):
        raise ValueError("egress hostname must not be internal")
    try:
        for label in value.split("."):
            _dns_label(label)
    except ValueError:
        raise ValueError("egress hostname must be an exact public DNS hostname") from None


def _profile(value):
    if value not in ("build", "claude-code"):
        raise ValueError("workbench profile must be build or claude-code")
    return value


def profile_of(template):
    values = [env.get("value") for container in template.get("containers", [])
              for env in container.get("env", []) if env.get("name") == "BROOD_WORKBENCH_PROFILE"]
    if len(values) > 1:
        raise ValueError("template has ambiguous workbench profile markers")
    return _profile(values[0] if values else "build")


def actor_template(image, atespace, name=_DEFAULT_NAME, profile="build"):
    """Шаблон API: запуск задания только явно, без автоматического golden actor."""
    _profile(profile)
    if name is _DEFAULT_NAME:
        name = "brood-claude-code" if profile == "claude-code" else "brood-build-test-deploy"
    _image(image)
    _dns_label(atespace)
    _dns_label(name)
    result = {
        "metadata": {"atespace": atespace, "name": name},
        "workerSelector": {"matchLabels": {"brood.dev/purpose": "development"}},
        "resources": {"limits": {"cpuMillicores": "4000", "memoryBytes": "8589934592"}},
        "snapshotConfig": {
            "preferredFidelity": "SNAPSHOT_FIDELITY_MEMORY",
            "goldenSnapshotConfig": {"mode": "GOLDEN_SNAPSHOT_MODE_DISABLED"}},
        "containers": [{
            "name": "main", "image": image,
            "command": ["/usr/bin/tini", "--", "/opt/seeds/brood/bin/brood", "workbench", "start"],
            "args": ["--", "bash", ".ci/workbench.sh"],
            "env": [{"name": key, "value": value} for key, value in {
                "HOME": "/workspace/home", "XDG_DATA_HOME": "/workspace/data",
                "SUBSTRATE_SOURCE": "/workspace/substrate", "TMPDIR": "/workspace/tmp",
                "DOCKER_HOST": "unix:///var/run/docker.sock", "PIP_FIND_LINKS": "/opt/wheels",
                "PIP_NO_INDEX": "1"}.items()],
            "volumeMounts": [{"name": "workspace", "mountPath": "/workspace"}],
            "securityContext": {"capabilities": {"add": list(GUEST_CAPABILITIES)}},
        }],
        "volumes": [{"name": "workspace", "durableDir": {}}],
    }
    if profile == "claude-code":
        container = result["containers"][0]
        container["args"] = ["--results-port", "8081", "--", "bash", "-c",
            "claude --version && exec ttyd --writable --check-origin --port 80 tmux new-session -A -s claude claude"]
        container["env"].extend({"name": key, "value": value} for key, value in {
            "BROOD_WORKBENCH_PROFILE": profile, "DISABLE_AUTOUPDATER": "1", "DISABLE_UPDATES": "1",
            "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", "CLAUDE_CODE_DISABLE_ARTIFACT": "1",
            "ENABLE_CLAUDEAI_MCP_SERVERS": "false"}.items())
    return result


def worker_pool(worker_image):
    """Отдельный microVM-пул с меткой для селектора шаблона разработки."""
    _image(worker_image)
    return {
        "apiVersion": "ate.dev/v1", "kind": "WorkerPool",
        "metadata": {"name": "brood-development", "namespace": "brood",
                     "labels": {"brood.dev/purpose": "development"}},
        "spec": {
            "replicas": 1, "workerImage": worker_image,
            "sandboxClasses": [{"name": "microvm", "configRef": {"name": "microvm"}}],
            "template": {
                "labels": {"brood.dev/purpose": "development"},
                "nodeSelector": {"kubernetes.io/arch": "amd64"},
                "resources": {"requests": {"cpu": "2", "memory": "8Gi"},
                              "limits": {"cpu": "6", "memory": "10Gi"}}},
        },
    }


def egress_policy(atespace, actor, hosts=PUBLIC_HOSTS, profile="build"):
    """Политика API; родительский actor передаётся отдельно при создании."""
    _profile(profile)
    _dns_label(atespace)
    _dns_label(actor)
    if not isinstance(hosts, (tuple, list)) or not 1 <= len(hosts) <= 256:
        raise ValueError("egress hosts must contain 1 to 256 exact public hostnames")
    hosts = tuple(hosts) + (CLAUDE_HOSTS if profile == "claude-code" else ())
    if len(hosts) > 256:
        raise ValueError("egress hosts must contain at most 256 exact public hostnames")
    for host in hosts:
        _hostname(host)
    return {
        "metadata": {"atespace": atespace, "name": "default"},
        "rules": [
            {"tlsPassthrough": {"hostnames": list(dict.fromkeys(hosts)),
                                "ports": {"numbers": [443]}}},
            {"http": {"hostnames": ["deb.debian.org", "security.debian.org"],
                      "ports": {"numbers": [80]}}},
        ],
    }
