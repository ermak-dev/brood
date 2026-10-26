"""Сборка development-образа из чистых Git-снимков без настроек оператора."""
import re
import shutil
import subprocess
import tempfile
import urllib.parse
from pathlib import Path

from brood.common import config
from brood.dev.deploy import server
from brood.dev.substrate import local


HERE = Path(__file__).resolve().parent


def version_pins(ci, provision):
    patterns = {
        "PYTHON_IMAGE": (ci, r'^IMAGE="([^"\n]+@sha256:[0-9a-f]{64})"$'),
        "GO_VERSION": (provision, r'^GO_VERSION="([0-9]+\.[0-9]+\.[0-9]+)"$'),
        "GO_SHA256": (provision, r'^GO_SHA256="([0-9a-f]{64})"$'),
    }
    pins = {}
    for name, (text, pattern) in patterns.items():
        match = re.search(pattern, text, re.M)
        if not match:
            raise RuntimeError(f"workbench needs the project pin {name}")
        pins[name] = match[1]
    return pins


def render_dockerfile(text, pins):
    for name, value in pins.items():
        marker = f"ARG {name}\n"
        if text.count(marker) != 1:
            raise RuntimeError(f"workbench Dockerfile needs exactly one {name} pin")
        text = text.replace(marker, f"ARG {name}={value}\n")
    return text


def _git(root, *args):
    result = subprocess.run(["git", "-C", str(root), *args], capture_output=True, text=True)
    if result.returncode:
        raise RuntimeError(f"workbench git {args[0]}: {result.stderr.strip()}")
    return result.stdout.strip()


def snapshot(source, ref, target):
    """Shallow fetch сохраняет настоящий commit, не копируя .env и локальный Git config."""
    source, target = Path(source).resolve(), Path(target)
    sha = _git(source, "rev-parse", "--verify", "--end-of-options", f"{ref}^{{commit}}")
    tracked = _git(source, "ls-tree", "-r", "--name-only", sha).splitlines()
    if any(path == ".env" or path.startswith("secrets/") or path == ".ate-dev-env.sh"
           for path in tracked):
        raise RuntimeError("workbench seed commit contains installation settings or secrets")
    origin = _git(source, "remote", "get-url", "origin")
    parsed = urllib.parse.urlsplit(origin)
    if parsed.password or (parsed.scheme in ("http", "https") and parsed.username):
        raise RuntimeError("workbench seed origin must not contain credentials")
    target.mkdir(parents=True)
    _git(target, "init", "-q")
    _git(target, "fetch", "-q", "--depth=1", "--update-shallow", str(source), sha)
    _git(target, "checkout", "-q", "--detach", "FETCH_HEAD")
    _git(target, "remote", "add", "origin", origin)
    (target / ".git/FETCH_HEAD").unlink(missing_ok=True)
    return sha


class Builder:
    """Собственный реестр и cluster ownership проверяются до публикации образа."""

    def __init__(self, cluster, project=config.PROJECT):
        self.cluster, self.project = cluster, Path(project)

    def context(self, directory, ref="HEAD"):
        directory = Path(directory).resolve()
        if directory == HERE.resolve() or HERE.resolve() in directory.parents:
            raise RuntimeError("build context must be outside the workbench source directory")
        directory.mkdir(parents=True, exist_ok=True)
        revision = snapshot(self.project, ref, directory / "brood")
        snapshot(self.cluster.source, local.COMMIT, directory / "substrate")
        pins = version_pins((directory / "brood/.ci/test.sh").read_text(),
                            (directory / "brood/deploy/provision.sh").read_text())
        (directory / "Dockerfile").write_text(render_dockerfile((HERE / "Dockerfile").read_text(), pins))
        shutil.copyfile(HERE / "Dockerfile.dockerignore", directory / "Dockerfile.dockerignore")
        shutil.copytree(HERE, directory / "runtime", ignore=shutil.ignore_patterns("__pycache__"))
        shutil.copytree(HERE, directory / "brood/brood/dev/workbench", dirs_exist_ok=True,
                        ignore=shutil.ignore_patterns("__pycache__"))
        shutil.copyfile(self.project / ".ci/workbench.sh", directory / "workbench.sh")
        shutil.copyfile(self.project / ".ci/workbench.sh", directory / "brood/.ci/workbench.sh")
        return revision

    def build(self, ref="HEAD"):
        self.cluster.check()
        code, architecture, err = self.cluster.shell.output("docker", "info", "--format", "{{.Architecture}}")
        if code or architecture.strip() not in ("x86_64", "amd64"):
            raise RuntimeError("workbench image currently requires an amd64 Docker host: " +
                               local.reason(err, code))
        with tempfile.TemporaryDirectory(prefix="brood-workbench.") as directory:
            self.context(directory, ref)
            digest = server.Deploy(self.cluster, project=directory)._image(
                "brood-workbench", str(Path(directory) / "Dockerfile"))
        return f"{server.REGISTRY}/brood-workbench@{digest}"
